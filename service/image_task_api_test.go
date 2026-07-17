package service

import (
	"testing"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover the §6.1 projection logic that turns the durable
// execution row into the public object. The DB interactions behind
// GetImageTask / CancelImageTask are exercised at the model layer
// (image_task_query_test.go); here we assert the state mapping, the
// result/error body placement, and the manual_review retry-after rule.

func TestProjectImageTaskPublicStatus_MapsAllExecutionStates(t *testing.T) {
	cases := []struct {
		exec model.ImageTaskExecutionState
		pub  dto.ImageTaskPublicStatus
	}{
		{model.ImageTaskStateQueued, dto.ImageTaskStatusQueued},
		{model.ImageTaskStateSubmitting, dto.ImageTaskStatusInProgress},
		{model.ImageTaskStateSubmissionUnknown, dto.ImageTaskStatusInProgress},
		{model.ImageTaskStatePolling, dto.ImageTaskStatusInProgress},
		{model.ImageTaskStateCompleted, dto.ImageTaskStatusCompleted},
		{model.ImageTaskStateFailed, dto.ImageTaskStatusFailed},
		{model.ImageTaskStateCancelRequested, dto.ImageTaskStatusCancelRequested},
		{model.ImageTaskStateCancelled, dto.ImageTaskStatusCancelled},
		{model.ImageTaskStateManualReview, dto.ImageTaskStatusManualReview},
	}
	for _, tc := range cases {
		t.Run(string(tc.exec), func(t *testing.T) {
			assert.Equal(t, tc.pub, projectImageTaskPublicStatus(tc.exec))
		})
	}
}

func TestProjectImageTaskPublicStatus_UnknownStateFoldsToFailed(t *testing.T) {
	// An unrecognized state is a data-integrity issue; surfacing it as failed
	// avoids leaking an internal name (§6.1: only the seven public statuses
	// leave the API).
	assert.Equal(t, dto.ImageTaskStatusFailed, projectImageTaskPublicStatus("bogus_state"))
}

func TestProjectImageTaskObject_PopulatesResultOnlyOnCompleted(t *testing.T) {
	result := model.ImageTaskResult{ContentURL: "https://x/y.png", MimeType: "image/png", SizeBytes: 10}

	completed := &model.ImageTaskExecution{
		PublicTaskID: "imgtask_1", State: model.ImageTaskStateCompleted, Result: result,
		CreatedAt: 100, UpdatedAt: 200,
	}
	obj := projectImageTaskObject(completed)
	assert.Equal(t, "imgtask_1", obj.ID)
	assert.Equal(t, imageTaskObjectKind, obj.Object)
	assert.Equal(t, dto.ImageTaskStatusCompleted, obj.Status)
	assert.Equal(t, int64(100), obj.CreatedAt)
	assert.Equal(t, int64(200), obj.UpdatedAt)
	require.NotNil(t, obj.Result)
	assert.Equal(t, "https://x/y.png", obj.Result.ContentURL)
	assert.Nil(t, obj.Error)

	// Completed but the processor has not stored a locator yet: no empty result.
	completedNoLocator := &model.ImageTaskExecution{PublicTaskID: "imgtask_2", State: model.ImageTaskStateCompleted}
	assert.Nil(t, projectImageTaskObject(completedNoLocator).Result)

	// Non-completed never carries a result even if the row has one.
	queued := &model.ImageTaskExecution{PublicTaskID: "imgtask_3", State: model.ImageTaskStateQueued, Result: result}
	assert.Nil(t, projectImageTaskObject(queued).Result)
}

func TestProjectImageTaskObject_PopulatesErrorOnlyOnFailed(t *testing.T) {
	failed := &model.ImageTaskExecution{PublicTaskID: "imgtask_4", State: model.ImageTaskStateFailed}
	obj := projectImageTaskObject(failed)
	require.NotNil(t, obj.Error)
	assert.Equal(t, imageTaskFailedCode, obj.Error.Code)
	assert.Nil(t, obj.Result)

	for _, s := range []model.ImageTaskExecutionState{
		model.ImageTaskStateQueued, model.ImageTaskStateCompleted, model.ImageTaskStateManualReview,
	} {
		exec := &model.ImageTaskExecution{PublicTaskID: "imgtask_" + string(s), State: s}
		assert.Nilf(t, projectImageTaskObject(exec).Error, "state %s should have no error body", s)
	}
}

func TestImageTaskRetryAfter_OnlyManualReview(t *testing.T) {
	assert.Equal(t, manualReviewRetryAfterSeconds, imageTaskRetryAfter(model.ImageTaskStateManualReview))
	for _, s := range []model.ImageTaskExecutionState{
		model.ImageTaskStateQueued, model.ImageTaskStatePolling, model.ImageTaskStateCompleted,
		model.ImageTaskStateFailed, model.ImageTaskStateCancelRequested, model.ImageTaskStateCancelled,
	} {
		assert.Zerof(t, imageTaskRetryAfter(s), "state %s should have no retry-after", s)
	}
}
