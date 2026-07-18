package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/system_setting"

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
	client := GetStrictSSRFProtectedHTTPClient()
	if client == nil {
		return ImageTaskResultDownload{}, errors.New("SSRF-protected result download client is not initialized")
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
		loc, lerr := locatorFromBlob(existing)
		if lerr != nil {
			return ImageTaskResult{}, resultStoreError(lerr)
		}
		return loc, nil
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
	loc, lerr := locatorFromBlob(stored)
	if lerr != nil {
		return ImageTaskResult{}, resultStoreError(lerr)
	}
	return loc, nil
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
	declared := strings.TrimSpace(strings.ToLower(strings.Split(download.ContentType, ";")[0]))
	sniffed := http.DetectContentType(download.Body)
	sniffed = strings.TrimSpace(strings.ToLower(strings.Split(sniffed, ";")[0]))
	if !strings.HasPrefix(sniffed, "image/") {
		return "", fmt.Errorf("image result bytes are %q, not an image (declared %q)", sniffed, download.ContentType)
	}
	if strings.HasPrefix(declared, "image/") && declared != sniffed {
		return "", fmt.Errorf("image result content type mismatch: declared %q, detected %q", declared, sniffed)
	}
	config, format, err := getImageConfig(bytes.NewReader(download.Body))
	if err != nil {
		return "", fmt.Errorf("decode image result dimensions: %w", err)
	}
	if err := validateImageTaskResultDimensions(config); err != nil {
		return "", err
	}
	expectedMIME := map[string]string{"jpeg": "image/jpeg", "png": "image/png", "gif": "image/gif", "webp": "image/webp"}[format]
	if expectedMIME == "" || expectedMIME != sniffed {
		return "", fmt.Errorf("image result decoder format %q does not match detected type %q", format, sniffed)
	}
	return sniffed, nil
}

func validateImageTaskResultDimensions(config image.Config) error {
	if config.Width <= 0 || config.Height <= 0 || config.Width > constant.ImageTaskResultMaxDimension || config.Height > constant.ImageTaskResultMaxDimension || int64(config.Width)*int64(config.Height) > constant.ImageTaskResultMaxPixels {
		return fmt.Errorf("image result dimensions %dx%d exceed safety limits", config.Width, config.Height)
	}
	return nil
}

// locatorFromBlob maps the durable blob row onto the execution's result locator.
// content_url points at new-api's own read path (served by GetImageTaskResult);
// expires_at is 0 because the stored copy is permanent, unlike the upstream URL.
// An error from the locator builder (WIS-572: missing/non-https ServerAddress)
// propagates so PersistImageTaskResult classifies it as result_store_error
// rather than emitting a path-only locator.
func locatorFromBlob(blob *model.ImageTaskResultBlob) (ImageTaskResult, error) {
	if blob == nil {
		return ImageTaskResult{}, nil
	}
	contentURL, err := imageTaskResultContentURL(blob.PublicTaskID)
	if err != nil {
		return ImageTaskResult{}, err
	}
	return ImageTaskResult{
		ContentURL: contentURL,
		MimeType:   blob.MimeType,
		SizeBytes:  blob.SizeBytes,
		SHA256:     blob.SHA256,
	}, nil
}

// imageTaskResultContentURL is the absolute https URL new-api serves the
// persisted blob from (GetImageTaskResult). WIS-572: the base is
// system_setting.ServerAddress (the same public base used for the symmetric
// video locator /v1/videos/{id}/content). The scheme is locked to https and a
// missing/malformed base fails fast — the previous path-only return was the
// root cause of CW's "content_url must be https" ingest break.
func imageTaskResultContentURL(publicTaskID string) (string, error) {
	if publicTaskID == "" {
		return "", nil
	}
	base, err := imageTaskResultBaseURL()
	if err != nil {
		return "", err
	}
	return base + "/v1/image-tasks/" + publicTaskID + "/result", nil
}

// imageTaskResultBaseURL resolves the public https base new-api exposes the
// image-task result endpoint at. It reuses system_setting.ServerAddress — the
// same public base the symmetric video locator builds on
// (taskcommon.BuildProxyURL → /v1/videos/{id}/content) — and locks the scheme
// to https. A missing, whitespace-only, malformed or non-https ServerAddress
// fails fast so the locator never degrades to a path-only or http value, which
// is the WIS-572 root cause: CW ValidateLocator rejects non-https, and a
// path-only locator has no host for the SSRF dial check to validate either.
func imageTaskResultBaseURL() (string, error) {
	raw := strings.TrimSpace(system_setting.ServerAddress)
	if raw == "" {
		return "", errors.New("image task result base URL not configured: set the public https system ServerAddress")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("image task result base URL invalid: %w", err)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("image task result base URL must be https, got scheme %q (configure system ServerAddress with an https public host)", u.Scheme)
	}
	if u.Host == "" {
		return "", errors.New("image task result base URL has empty host (configure system ServerAddress with a public https host)")
	}
	return strings.TrimRight(raw, "/"), nil
}
