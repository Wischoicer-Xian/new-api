package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests drive the GET/cancel handlers end-to-end through a gin test
// context against a per-test SQLite DB: they lock the owner-scoped read, the
// Retry-After header on manual_review, the error body on failed, and the
// idempotent cancel semantics. The projection mapping itself is asserted in
// service/image_task_api_test.go; here we verify the HTTP shape.

// setupImageTaskControllerDB initializes the shared controller-test SQLite DB
// (setupModelListControllerTestDB) and adds the image_task_executions table the
// handlers read from.
func setupImageTaskControllerDB(t *testing.T) {
	t.Helper()
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.ImageTaskExecution{}))
}

var imageTaskCtlSeq int64

func newCtlImageTaskExecution(t *testing.T, owner int, state model.ImageTaskExecutionState, result *model.ImageTaskResult) *model.ImageTaskExecution {
	t.Helper()
	seq := atomic.AddInt64(&imageTaskCtlSeq, 1)
	exec := &model.ImageTaskExecution{
		PublicTaskID:   fmt.Sprintf("imgtask_ctl_%d", seq),
		TaskDBID:       seq,
		OwnerUserID:    owner,
		Operation:      model.ImageTaskOperationGeneration,
		IdempotencyKey: fmt.Sprintf("ctl-k-%d", seq),
		RequestHash:    fmt.Sprintf("ctl-h-%d", seq),
		State:          state,
		CreatedAt:      1000,
		UpdatedAt:      2000,
	}
	if result != nil {
		exec.Result = *result
	}
	require.NoError(t, model.DB.Create(exec).Error)
	return exec
}

func doGetImageTask(taskID string, ownerID int) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/image-tasks/"+taskID, nil)
	c.Set("id", ownerID)
	c.Params = gin.Params{{Key: "task_id", Value: taskID}}
	GetImageTask(c)
	return w
}

func doCancelImageTask(taskID string, ownerID int) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/image-tasks/"+taskID+"/cancel", nil)
	c.Set("id", ownerID)
	c.Params = gin.Params{{Key: "task_id", Value: taskID}}
	CancelImageTask(c)
	return w
}

func parseImageTaskBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, common.Unmarshal(w.Body.Bytes(), &m))
	return m
}

func TestGetImageTask_HandlerReturnsOwnerTask(t *testing.T) {
	setupImageTaskControllerDB(t)
	exec := newCtlImageTaskExecution(t, 7001, model.ImageTaskStateQueued, nil)

	w := doGetImageTask(exec.PublicTaskID, 7001)
	require.Equal(t, http.StatusOK, w.Code)
	m := parseImageTaskBody(t, w)
	assert.Equal(t, exec.PublicTaskID, m["id"])
	assert.Equal(t, "image.task", m["object"])
	assert.Equal(t, "queued", m["status"])
	assert.Nil(t, m["result"])
	assert.Nil(t, m["error"])
}

func TestGetImageTask_HandlerOwnerMismatchIs404(t *testing.T) {
	setupImageTaskControllerDB(t)
	exec := newCtlImageTaskExecution(t, 7002, model.ImageTaskStateQueued, nil)

	w := doGetImageTask(exec.PublicTaskID, 9999)
	require.Equal(t, http.StatusNotFound, w.Code)
	m := parseImageTaskBody(t, w)
	assert.Equal(t, "NOT_FOUND", m["code"])
}

func TestGetImageTask_HandlerMissingIs404(t *testing.T) {
	setupImageTaskControllerDB(t)
	w := doGetImageTask("imgtask_missing", 7003)
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestGetImageTask_HandlerManualReviewSetsRetryAfter(t *testing.T) {
	setupImageTaskControllerDB(t)
	exec := newCtlImageTaskExecution(t, 7004, model.ImageTaskStateManualReview, nil)

	w := doGetImageTask(exec.PublicTaskID, 7004)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "300", w.Header().Get("Retry-After"))
	m := parseImageTaskBody(t, w)
	assert.Equal(t, "manual_review", m["status"])
}

func TestGetImageTask_HandlerCompletedCarriesResult(t *testing.T) {
	setupImageTaskControllerDB(t)
	result := &model.ImageTaskResult{ContentURL: "https://oss/x.png", MimeType: "image/png", SizeBytes: 42}
	exec := newCtlImageTaskExecution(t, 7005, model.ImageTaskStateCompleted, result)

	w := doGetImageTask(exec.PublicTaskID, 7005)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get("Retry-After"))
	m := parseImageTaskBody(t, w)
	assert.Equal(t, "completed", m["status"])
	r := m["result"].(map[string]any)
	assert.Equal(t, "https://oss/x.png", r["content_url"])
	assert.EqualValues(t, 42, r["size_bytes"])
}

func TestGetImageTask_HandlerFailedCarriesErrorBody(t *testing.T) {
	setupImageTaskControllerDB(t)
	exec := newCtlImageTaskExecution(t, 7006, model.ImageTaskStateFailed, nil)

	w := doGetImageTask(exec.PublicTaskID, 7006)
	require.Equal(t, http.StatusOK, w.Code)
	m := parseImageTaskBody(t, w)
	assert.Equal(t, "failed", m["status"])
	e := m["error"].(map[string]any)
	assert.Equal(t, "TASK_FAILED", e["code"])
}

func TestCancelImageTask_HandlerTransitionsQueuedToCancelRequested(t *testing.T) {
	setupImageTaskControllerDB(t)
	exec := newCtlImageTaskExecution(t, 7007, model.ImageTaskStateQueued, nil)

	w := doCancelImageTask(exec.PublicTaskID, 7007)
	require.Equal(t, http.StatusOK, w.Code)
	m := parseImageTaskBody(t, w)
	assert.Equal(t, "cancel_requested", m["status"])

	// Persisted: the durable row reflects the transition.
	var reloaded model.ImageTaskExecution
	require.NoError(t, model.DB.First(&reloaded, exec.ID).Error)
	assert.Equal(t, model.ImageTaskStateCancelRequested, reloaded.State)
	assert.NotZero(t, reloaded.CancelRequestedAt)
}

func TestCancelImageTask_HandlerTerminalIsIdempotent(t *testing.T) {
	setupImageTaskControllerDB(t)
	exec := newCtlImageTaskExecution(t, 7008, model.ImageTaskStateCompleted,
		&model.ImageTaskResult{ContentURL: "https://oss/y.png"})

	w := doCancelImageTask(exec.PublicTaskID, 7008)
	require.Equal(t, http.StatusOK, w.Code)
	m := parseImageTaskBody(t, w)
	// Terminal task is unchanged; result is still served.
	assert.Equal(t, "completed", m["status"])
}

func TestCancelImageTask_HandlerMissingIs404(t *testing.T) {
	setupImageTaskControllerDB(t)
	w := doCancelImageTask("imgtask_missing", 7009)
	require.Equal(t, http.StatusNotFound, w.Code)
}
