package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"testing"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// pngFixture is an 8x8 PNG; http.DetectContentType reports image/png for it.
var pngFixture = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d,
	0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x04, 0x00, 0x00, 0x00, 0xb5, 0x1c, 0x0c, 0x02, 0x00, 0x00, 0x00,
	0x0b, 0x49, 0x44, 0x41, 0x54, 0x78, 0xda, 0x63, 0x64, 0xf8, 0x0f, 0x00,
	0x01, 0x05, 0x01, 0x01, 0x27, 0x18, 0xe3, 0x66, 0x00, 0x00, 0x00, 0x00,
	0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
}

func seedExecutionForResultStore(t *testing.T) *model.ImageTaskExecution {
	t.Helper()
	exec := &model.ImageTaskExecution{
		PublicTaskID: "imgtask_result_1", TaskDBID: 901, OwnerUserID: 1,
		Operation: "generation", State: model.ImageTaskStatePolling, CreatedAt: 1, UpdatedAt: 1,
	}
	require.NoError(t, model.DB.Create(exec).Error)
	return exec
}

func useResultDownloader(t *testing.T, download ImageTaskResultDownload, downloadErr error) {
	t.Helper()
	original := imageTaskResultDownloader
	imageTaskResultDownloader = func(context.Context, string) (ImageTaskResultDownload, error) {
		return download, downloadErr
	}
	t.Cleanup(func() { imageTaskResultDownloader = original })
}

// useImageTaskResultBaseURL pins system_setting.ServerAddress for one test and
// restores it on cleanup. imageTaskResultContentURL reads ServerAddress as the
// public base; tests must set it explicitly because the package default is an
// http://localhost placeholder that the new https-only contract rejects.
func useImageTaskResultBaseURL(t *testing.T, serverAddress string) {
	t.Helper()
	original := system_setting.ServerAddress
	system_setting.ServerAddress = serverAddress
	t.Cleanup(func() { system_setting.ServerAddress = original })
}

func TestPersistImageTaskResultDownloadsAndStores(t *testing.T) {
	truncate(t)
	exec := seedExecutionForResultStore(t)
	useResultDownloader(t, ImageTaskResultDownload{Body: pngFixture, ContentType: "image/png"}, nil)
	useImageTaskResultBaseURL(t, "https://newapi.example.com")

	locator, err := PersistImageTaskResult(context.Background(), exec, "https://fixtures.example.com/img.png")
	require.NoError(t, err)
	assert.Equal(t, "https://newapi.example.com/v1/image-tasks/imgtask_result_1/result", locator.ContentURL)
	assert.Equal(t, "image/png", locator.MimeType)
	assert.Equal(t, int64(len(pngFixture)), locator.SizeBytes)

	digest := sha256.Sum256(pngFixture)
	assert.Equal(t, hex.EncodeToString(digest[:]), locator.SHA256)

	// The blob row is durable.
	blob, err := model.GetImageTaskResultBlob(exec.ID)
	require.NoError(t, err)
	assert.Equal(t, pngFixture, blob.Content)
	assert.Equal(t, locator.SHA256, blob.SHA256)
}

func TestPersistImageTaskResultIdempotent(t *testing.T) {
	truncate(t)
	exec := seedExecutionForResultStore(t)
	useImageTaskResultBaseURL(t, "https://newapi.example.com")
	calls := 0
	original := imageTaskResultDownloader
	imageTaskResultDownloader = func(context.Context, string) (ImageTaskResultDownload, error) {
		calls++
		return ImageTaskResultDownload{Body: pngFixture, ContentType: "image/png"}, nil
	}
	t.Cleanup(func() { imageTaskResultDownloader = original })

	first, err := PersistImageTaskResult(context.Background(), exec, "https://fixtures.example.com/img.png")
	require.NoError(t, err)
	second, err := PersistImageTaskResult(context.Background(), exec, "https://fixtures.example.com/img.png")
	require.NoError(t, err)
	assert.Equal(t, first.SHA256, second.SHA256)
	assert.Equal(t, 1, calls, "idempotent re-persist must not re-download")
}

func TestPersistImageTaskResultSniffedImageType(t *testing.T) {
	truncate(t)
	exec := seedExecutionForResultStore(t)
	// Generic Content-Type but sniffable as image → sniffed MIME wins.
	useResultDownloader(t, ImageTaskResultDownload{Body: pngFixture, ContentType: "application/octet-stream"}, nil)
	useImageTaskResultBaseURL(t, "https://newapi.example.com")

	locator, err := PersistImageTaskResult(context.Background(), exec, "https://x")
	require.NoError(t, err)
	assert.Equal(t, "image/png", locator.MimeType)
}

func TestPersistImageTaskResultRejectsNonImage(t *testing.T) {
	truncate(t)
	exec := seedExecutionForResultStore(t)
	useResultDownloader(t, ImageTaskResultDownload{Body: []byte("plain text body"), ContentType: "text/plain"}, nil)

	_, err := PersistImageTaskResult(context.Background(), exec, "https://x")
	perr := AsImageProviderError(err)
	require.NotNil(t, err)
	require.NotNil(t, perr)
	assert.Equal(t, ImageErrResultStore, perr.Kind)
}

func TestPersistImageTaskResultRejectsSpoofedImageContentType(t *testing.T) {
	truncate(t)
	exec := seedExecutionForResultStore(t)
	useResultDownloader(t, ImageTaskResultDownload{Body: []byte("<html>not an image</html>"), ContentType: "image/png"}, nil)

	_, err := PersistImageTaskResult(context.Background(), exec, "https://x")
	perr := AsImageProviderError(err)
	require.NotNil(t, perr)
	assert.Equal(t, ImageErrResultStore, perr.Kind)
}

func TestPersistImageTaskResultRejectsOversizedDimensions(t *testing.T) {
	err := validateImageTaskResultDimensions(image.Config{Width: 65_535, Height: 65_535})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceed safety limits")
}

func TestPersistImageTaskResultRejectsEmpty(t *testing.T) {
	truncate(t)
	exec := seedExecutionForResultStore(t)
	useResultDownloader(t, ImageTaskResultDownload{Body: nil, ContentType: "image/png"}, nil)

	_, err := PersistImageTaskResult(context.Background(), exec, "https://x")
	perr := AsImageProviderError(err)
	require.NotNil(t, perr)
	assert.Equal(t, ImageErrResultStore, perr.Kind)
}

func TestPersistImageTaskResultDownloadFailureClassified(t *testing.T) {
	truncate(t)
	exec := seedExecutionForResultStore(t)
	useResultDownloader(t, ImageTaskResultDownload{}, errors.New("connection reset"))

	_, err := PersistImageTaskResult(context.Background(), exec, "https://x")
	perr := AsImageProviderError(err)
	require.NotNil(t, perr)
	assert.Equal(t, ImageErrResultStore, perr.Kind)
}

func TestCreateImageTaskResultBlobConflict(t *testing.T) {
	truncate(t)
	exec := seedExecutionForResultStore(t)
	first := &model.ImageTaskResultBlob{
		ExecutionID: exec.ID, PublicTaskID: exec.PublicTaskID,
		MimeType: "image/png", SizeBytes: 1, SHA256: "aaa", Content: []byte{1}, CreatedAt: 1,
	}
	created, stored, err := model.CreateImageTaskResultBlob(first)
	require.NoError(t, err)
	require.True(t, created)
	require.NotNil(t, stored)

	conflict := &model.ImageTaskResultBlob{
		ExecutionID: exec.ID, PublicTaskID: exec.PublicTaskID,
		MimeType: "image/png", SizeBytes: 2, SHA256: "bbb", Content: []byte{2}, CreatedAt: 2,
	}
	_, _, err = model.CreateImageTaskResultBlob(conflict)
	assert.ErrorIs(t, err, model.ErrImageTaskResultBlobConflict)
}

func TestCreateImageTaskResultBlobIdempotentReplay(t *testing.T) {
	truncate(t)
	exec := seedExecutionForResultStore(t)
	blob := &model.ImageTaskResultBlob{
		ExecutionID: exec.ID, PublicTaskID: exec.PublicTaskID,
		MimeType: "image/png", SizeBytes: 1, SHA256: "same", Content: []byte{9}, CreatedAt: 1,
	}
	created, _, err := model.CreateImageTaskResultBlob(blob)
	require.NoError(t, err)
	require.True(t, created)

	replay := &model.ImageTaskResultBlob{
		ExecutionID: exec.ID, PublicTaskID: exec.PublicTaskID,
		MimeType: "image/png", SizeBytes: 1, SHA256: "same", Content: []byte{9}, CreatedAt: 2,
	}
	created, stored, err := model.CreateImageTaskResultBlob(replay)
	require.NoError(t, err)
	require.False(t, created, "same-sha replay is a benign no-op")
	require.NotNil(t, stored)
	assert.Equal(t, "same", stored.SHA256)
}

func TestGetImageTaskResultBlobMissing(t *testing.T) {
	truncate(t)
	_, err := model.GetImageTaskResultBlob(999999)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

// TestImageTaskResultContentURL covers the locator builder directly (no DB).
// WIS-572: the locator must be an absolute https URL built on
// system_setting.ServerAddress; a missing or non-https base must fail fast
// rather than emitting a path-only/http locator (which CW ValidateLocator
// rejects and was the original ingest break).
func TestImageTaskResultContentURL(t *testing.T) {
	cases := []struct {
		name        string
		serverAddr  string
		publicTask  string
		wantURL     string
		wantErr     bool
		errContains string
	}{
		{name: "https absolute", serverAddr: "https://newapi.example.com", publicTask: "imgtask_x", wantURL: "https://newapi.example.com/v1/image-tasks/imgtask_x/result"},
		{name: "https trailing slash trimmed", serverAddr: "https://newapi.example.com/", publicTask: "imgtask_x", wantURL: "https://newapi.example.com/v1/image-tasks/imgtask_x/result"},
		{name: "https with port", serverAddr: "https://newapi.example.com:8443", publicTask: "imgtask_x", wantURL: "https://newapi.example.com:8443/v1/image-tasks/imgtask_x/result"},
		{name: "empty publicTaskID fails fast", serverAddr: "https://newapi.example.com", publicTask: "", wantErr: true, errContains: "public task id"},
		{name: "missing ServerAddress fails fast", serverAddr: "", publicTask: "imgtask_x", wantErr: true, errContains: "ServerAddress"},
		{name: "whitespace-only ServerAddress fails fast", serverAddr: "   ", publicTask: "imgtask_x", wantErr: true, errContains: "ServerAddress"},
		{name: "http ServerAddress fails fast (https locked)", serverAddr: "http://newapi.example.com", publicTask: "imgtask_x", wantErr: true, errContains: "https"},
		{name: "schemeless ServerAddress fails fast", serverAddr: "newapi.example.com", publicTask: "imgtask_x", wantErr: true},
		// P2 (WIS-572 review): ServerAddress must be a clean public origin.
		// userinfo would leak credentials into the locator; query/fragment would
		// push the result path into the wrong route. Rebuilt from parsed components.
		{name: "rejects userinfo", serverAddr: "https://user:pass@newapi.example.com", publicTask: "imgtask_x", wantErr: true, errContains: "userinfo"},
		{name: "rejects query string", serverAddr: "https://newapi.example.com?x=1", publicTask: "imgtask_x", wantErr: true, errContains: "query"},
		{name: "rejects fragment", serverAddr: "https://newapi.example.com#frag", publicTask: "imgtask_x", wantErr: true, errContains: "fragment"},
		{name: "allows subpath origin (reverse-proxy deploy)", serverAddr: "https://newapi.example.com/newapi", publicTask: "imgtask_x", wantURL: "https://newapi.example.com/newapi/v1/image-tasks/imgtask_x/result"},
		{name: "normalizes subpath trailing slash", serverAddr: "https://newapi.example.com/newapi/", publicTask: "imgtask_x", wantURL: "https://newapi.example.com/newapi/v1/image-tasks/imgtask_x/result"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useImageTaskResultBaseURL(t, tc.serverAddr)
			got, err := imageTaskResultContentURL(tc.publicTask)
			if tc.wantErr {
				require.Error(t, err)
				assert.Empty(t, got, "fail-fast must not emit a usable path-only/http locator")
				if tc.errContains != "" {
					assert.Contains(t, err.Error(), tc.errContains)
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantURL, got)
		})
	}
}

// TestPersistImageTaskResultFailsFastWhenBaseURLMissing asserts a missing
// ServerAddress surfaces as a result_store_error instead of a path-only
// locator. The blob is still persisted, so provisioning ServerAddress later
// recovers without re-downloading.
func TestPersistImageTaskResultFailsFastWhenBaseURLMissing(t *testing.T) {
	truncate(t)
	exec := seedExecutionForResultStore(t)
	useResultDownloader(t, ImageTaskResultDownload{Body: pngFixture, ContentType: "image/png"}, nil)
	useImageTaskResultBaseURL(t, "")

	_, err := PersistImageTaskResult(context.Background(), exec, "https://fixtures.example.com/img.png")
	perr := AsImageProviderError(err)
	require.NotNil(t, perr, "missing base URL must surface as a provider error")
	assert.Equal(t, ImageErrResultStore, perr.Kind)
	assert.Contains(t, err.Error(), "ServerAddress")

	// Blob was persisted before the locator build; provisioning recovers later.
	blob, err := model.GetImageTaskResultBlob(exec.ID)
	require.NoError(t, err)
	assert.Equal(t, pngFixture, blob.Content, "bytes must be durable even when the locator cannot be built")
}

// TestPersistImageTaskResultFailsFastWhenBaseURLNotHTTPS asserts an http
// ServerAddress is rejected (https locked), so the locator never degrades to
// an http URL CW would also reject.
func TestPersistImageTaskResultFailsFastWhenBaseURLNotHTTPS(t *testing.T) {
	truncate(t)
	exec := seedExecutionForResultStore(t)
	useResultDownloader(t, ImageTaskResultDownload{Body: pngFixture, ContentType: "image/png"}, nil)
	useImageTaskResultBaseURL(t, "http://newapi.example.com")

	_, err := PersistImageTaskResult(context.Background(), exec, "https://fixtures.example.com/img.png")
	perr := AsImageProviderError(err)
	require.NotNil(t, perr)
	assert.Equal(t, ImageErrResultStore, perr.Kind)
}
