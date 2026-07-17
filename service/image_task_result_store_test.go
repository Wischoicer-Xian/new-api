package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// pngFixture is an 8x8 PNG; http.DetectContentType reports image/png for it.
var pngFixture = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, // PNG signature
	0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
	0x89,
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

func TestPersistImageTaskResultDownloadsAndStores(t *testing.T) {
	truncate(t)
	exec := seedExecutionForResultStore(t)
	useResultDownloader(t, ImageTaskResultDownload{Body: pngFixture, ContentType: "image/png"}, nil)

	locator, err := PersistImageTaskResult(context.Background(), exec, "https://fixtures.example.com/img.png")
	require.NoError(t, err)
	assert.Equal(t, "/v1/image-tasks/imgtask_result_1/result", locator.ContentURL)
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
