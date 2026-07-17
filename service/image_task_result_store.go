package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"

	"gorm.io/gorm"
)

// ImageTaskResult is the durable locator the processor writes onto the execution
// row once a completed image is persisted. It mirrors model.ImageTaskResult so
// the processor can assign it without a second mapping; the alias keeps the
// service API self-documenting.
type ImageTaskResult = model.ImageTaskResult

// ImageTaskResultDownload is the raw bytes fetched from the upstream temporary
// URL plus the content type the server declared. The store validates both
// before persisting.
type ImageTaskResultDownload struct {
	Body        []byte
	ContentType string
}

// imageTaskResultDownloader fetches the bytes at the upstream download URL.
// Production uses the SSRF-protected client (the URL is provider-served but
// still an external fetch); tests override this seam to return fixture bytes.
var imageTaskResultDownloader = defaultImageTaskResultDownloader

func defaultImageTaskResultDownloader(ctx context.Context, url string) (ImageTaskResultDownload, error) {
	if url == "" {
		return ImageTaskResultDownload{}, errors.New("image task result download url is empty")
	}
	client := GetSSRFProtectedHTTPClient()
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ImageTaskResultDownload{}, fmt.Errorf("build result download request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return ImageTaskResultDownload{}, fmt.Errorf("download image result: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ImageTaskResultDownload{}, fmt.Errorf("download image result: upstream status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, constant.ImageTaskResultMaxBytes+1))
	if err != nil {
		return ImageTaskResultDownload{}, fmt.Errorf("read image result body: %w", err)
	}
	return ImageTaskResultDownload{Body: body, ContentType: resp.Header.Get("Content-Type")}, nil
}

// PersistImageTaskResult downloads (unless a blob already exists for the
// execution), validates, and durably stores one completed image. It is the
// §7.6 result store: the upstream temporary URL is fetched exactly once per
// execution, the bytes are validated (image MIME, bounded size, sha256), and the
// durable locator is returned for the processor to write onto the execution row.
//
// Idempotency: if a blob already exists for the execution, it is returned
// without re-downloading. A concurrent store races on the execution_id unique
// index; the loser re-reads the winner. A re-download that yields different
// bytes (different sha256) is a conflict and surfaces as result_store_error so
// the processor retries rather than silently overwriting.
//
// On any failure the returned error wraps a result_store_error-classified
// ImageProviderError so the processor's error budget applies uniformly.
func PersistImageTaskResult(ctx context.Context, exec *model.ImageTaskExecution, downloadURL string) (ImageTaskResult, error) {
	if exec == nil {
		return ImageTaskResult{}, resultStoreError(errors.New("nil execution"))
	}
	if existing, err := model.GetImageTaskResultBlob(exec.ID); err == nil && existing != nil {
		return locatorFromBlob(existing), nil
	} else if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		// A real DB error (not record-not-found) is a result_store_error.
		return ImageTaskResult{}, resultStoreError(fmt.Errorf("load existing result blob: %w", err))
	}

	download, err := imageTaskResultDownloader(ctx, downloadURL)
	if err != nil {
		return ImageTaskResult{}, resultStoreError(err)
	}
	mime, verr := validateImageTaskResultDownload(download)
	if verr != nil {
		return ImageTaskResult{}, resultStoreError(verr)
	}
	digest := sha256.Sum256(download.Body)
	blob := &model.ImageTaskResultBlob{
		ExecutionID:  exec.ID,
		PublicTaskID: exec.PublicTaskID,
		MimeType:     mime,
		SizeBytes:    int64(len(download.Body)),
		SHA256:       hex.EncodeToString(digest[:]),
		Content:      append([]byte(nil), download.Body...),
		CreatedAt:    common.GetTimestamp(),
	}
	_, stored, err := model.CreateImageTaskResultBlob(blob)
	if err != nil {
		return ImageTaskResult{}, resultStoreError(fmt.Errorf("persist result blob: %w", err))
	}
	return locatorFromBlob(stored), nil
}

// validateImageTaskResultDownload enforces the §7.6 invariants: the bytes must
// be an image MIME (declared or sniffed) and within the configured size bound.
// Sniffing covers providers that declare a generic Content-Type; the sniffed
// type wins only when the declared type is absent or non-image.
func validateImageTaskResultDownload(download ImageTaskResultDownload) (string, error) {
	if len(download.Body) == 0 {
		return "", errors.New("image result body is empty")
	}
	if int64(len(download.Body)) > constant.ImageTaskResultMaxBytes {
		return "", fmt.Errorf("image result body exceeds %d bytes", constant.ImageTaskResultMaxBytes)
	}
	mime := strings.TrimSpace(strings.ToLower(strings.Split(download.ContentType, ";")[0]))
	if !strings.HasPrefix(mime, "image/") {
		sniffed := http.DetectContentType(download.Body)
		sniffed = strings.TrimSpace(strings.ToLower(strings.Split(sniffed, ";")[0]))
		if strings.HasPrefix(sniffed, "image/") {
			mime = sniffed
		} else {
			return "", fmt.Errorf("image result content type %q is not an image", download.ContentType)
		}
	}
	return mime, nil
}

// locatorFromBlob maps the durable blob row onto the execution's result locator.
// content_url points at new-api's own read path (served by a future handler);
// expires_at is 0 because the stored copy is permanent, unlike the upstream URL.
func locatorFromBlob(blob *model.ImageTaskResultBlob) ImageTaskResult {
	if blob == nil {
		return ImageTaskResult{}
	}
	return ImageTaskResult{
		ContentURL: imageTaskResultContentURL(blob.PublicTaskID),
		MimeType:   blob.MimeType,
		SizeBytes:  blob.SizeBytes,
		SHA256:     blob.SHA256,
	}
}

// imageTaskResultContentURL is the stable path new-api will serve the persisted
// blob from. The handler lands in a later phase; until then the locator is
// durable (the bytes are in the result store) and the URL is deterministic.
func imageTaskResultContentURL(publicTaskID string) string {
	if publicTaskID == "" {
		return ""
	}
	return "/v1/image-tasks/" + publicTaskID + "/result"
}
