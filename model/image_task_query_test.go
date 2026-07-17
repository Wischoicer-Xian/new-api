package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// These tests cover the public read/cancel entry points the image-task API
// handlers call: GetImageTaskExecutionByPublicTaskID (GET) and
// RequestImageTaskCancelCAS (cancel). Creation, idempotency convergence, and
// the terminal finalize CAS are exercised in image_task_execution_test.go.

func TestGetImageTaskExecutionByPublicTaskID_ScopedToOwner(t *testing.T) {
	truncateImageTaskExecutions(t)
	exec := newImageTaskExecution(9001, ImageTaskOperationGeneration, "q-owner", "hash")
	require.NoError(t, DB.Create(exec).Error)

	got, err := GetImageTaskExecutionByPublicTaskID(exec.PublicTaskID, 9001)
	require.NoError(t, err)
	assert.Equal(t, exec.ID, got.ID)

	// A different owner cannot read it; the miss is reported as not-found so
	// no cross-account existence leaks through the GET handler (§6.1).
	_, err = GetImageTaskExecutionByPublicTaskID(exec.PublicTaskID, 9999)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestGetImageTaskExecutionByPublicTaskID_MissingIsNotFound(t *testing.T) {
	truncateImageTaskExecutions(t)
	_, err := GetImageTaskExecutionByPublicTaskID("imgtask_missing", 9001)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestRequestImageTaskCancelCAS_TransitionsNonTerminalToCancelRequested(t *testing.T) {
	truncateImageTaskExecutions(t)
	exec := newImageTaskExecution(9002, ImageTaskOperationGeneration, "q-queued", "hash")
	require.NoError(t, DB.Create(exec).Error)

	won, got, err := RequestImageTaskCancelCAS(exec.PublicTaskID, 9002, 1700000000)
	require.NoError(t, err)
	assert.True(t, won)
	assert.Equal(t, ImageTaskStateCancelRequested, got.State)
	assert.Equal(t, int64(1700000000), got.CancelRequestedAt)

	var reloaded ImageTaskExecution
	require.NoError(t, DB.First(&reloaded, exec.ID).Error)
	assert.Equal(t, ImageTaskStateCancelRequested, reloaded.State)
	assert.Equal(t, int64(1700000000), reloaded.CancelRequestedAt)
}

func TestRequestImageTaskCancelCAS_TerminalIsIdempotentNoOp(t *testing.T) {
	truncateImageTaskExecutions(t)
	for _, terminal := range []ImageTaskExecutionState{ImageTaskStateCompleted, ImageTaskStateFailed, ImageTaskStateCancelled} {
		exec := newImageTaskExecution(9003, ImageTaskOperationGeneration, "q-term-"+string(terminal), "hash")
		exec.State = terminal
		require.NoError(t, DB.Create(exec).Error)

		won, got, err := RequestImageTaskCancelCAS(exec.PublicTaskID, 9003, 1700000001)
		require.NoError(t, err)
		assert.Falsef(t, won, "terminal state %s should not transition", terminal)
		assert.Equal(t, terminal, got.State)

		// State is unchanged on disk.
		var reloaded ImageTaskExecution
		require.NoError(t, DB.First(&reloaded, exec.ID).Error)
		assert.Equal(t, terminal, reloaded.State)
		assert.Zero(t, reloaded.CancelRequestedAt)
	}
}

func TestRequestImageTaskCancelCAS_AlreadyCancelRequestedIsIdempotent(t *testing.T) {
	truncateImageTaskExecutions(t)
	exec := newImageTaskExecution(9004, ImageTaskOperationGeneration, "q-again", "hash")
	exec.State = ImageTaskStateCancelRequested
	exec.CancelRequestedAt = 1700000000
	require.NoError(t, DB.Create(exec).Error)

	won, got, err := RequestImageTaskCancelCAS(exec.PublicTaskID, 9004, 1700000099)
	require.NoError(t, err)
	assert.False(t, won)
	assert.Equal(t, ImageTaskStateCancelRequested, got.State)
	// The no-op must not restamp the original cancel timestamp.
	assert.Equal(t, int64(1700000000), got.CancelRequestedAt)
}

func TestRequestImageTaskCancelCAS_ManualReviewIsCancellable(t *testing.T) {
	truncateImageTaskExecutions(t)
	// manual_review is non-terminal and cancellable per §6.1 ("裁决前允许取消").
	exec := newImageTaskExecution(9005, ImageTaskOperationGeneration, "q-review", "hash")
	exec.State = ImageTaskStateManualReview
	require.NoError(t, DB.Create(exec).Error)

	won, got, err := RequestImageTaskCancelCAS(exec.PublicTaskID, 9005, 1700000200)
	require.NoError(t, err)
	assert.True(t, won)
	assert.Equal(t, ImageTaskStateCancelRequested, got.State)
}

func TestRequestImageTaskCancelCAS_OwnerMismatchIsNotFound(t *testing.T) {
	truncateImageTaskExecutions(t)
	exec := newImageTaskExecution(9006, ImageTaskOperationGeneration, "q-owner2", "hash")
	require.NoError(t, DB.Create(exec).Error)

	won, _, err := RequestImageTaskCancelCAS(exec.PublicTaskID, 9999, 1700000300)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
	assert.False(t, won)

	// The rightful owner's task is untouched.
	var reloaded ImageTaskExecution
	require.NoError(t, DB.First(&reloaded, exec.ID).Error)
	assert.Equal(t, ImageTaskStateQueued, reloaded.State)
}
