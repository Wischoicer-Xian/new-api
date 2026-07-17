package service

import (
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"gorm.io/gorm"
)

// imageTaskObjectKind is the value of ImageTaskObject.Object on every §6.1
// response. Frozen by the contract so clients can dispatch on it.
const imageTaskObjectKind = "image.task"

// manualReviewRetryAfterSeconds is the Retry-After hint served alongside a
// manual_review status (§6.1: "查询响应附带长间隔 Retry-After"). It is long
// because the state only clears via an audited operator ruling, not polling.
const manualReviewRetryAfterSeconds = 300

// imageTaskFailedCode is the machine code placed in a terminal-failed task's
// error body. The execution row does not yet carry a public failure message
// (the processor writes detailed reasons in §7.5); clients see a stable code.
const imageTaskFailedCode = "TASK_FAILED"

// GetImageTask loads a user's image task by public id and projects it onto the
// §6.1 public object. A missing or cross-account task yields a 404
// ImageTaskRequestError so the handler returns NOT_FOUND without leaking
// existence across accounts. The read never touches the provider (§6.1).
//
// retryAfter is non-zero only for manual_review, where the handler emits it as
// a Retry-After header; it is zero for every other state.
func GetImageTask(ownerUserID int, publicTaskID string) (obj *dto.ImageTaskObject, retryAfter int, err error) {
	exec, err := model.GetImageTaskExecutionByPublicTaskID(publicTaskID, ownerUserID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, 0, imageTaskNotFound()
		}
		return nil, 0, fmt.Errorf("get image task %s for user %d: %w", publicTaskID, ownerUserID, err)
	}
	return projectImageTaskObject(exec), imageTaskRetryAfter(exec.State), nil
}

// CancelImageTask records a cancel request on a non-terminal task. The actual
// provider cancellation and terminal settle/refund are the processor's job
// (§9.2 cancel_guard: once cancel_requested, submit/failover and
// next-generation are forbidden). Returns:
//   - object: the current public projection (always, so the handler can show
//     the present state whether or not a transition ran);
//   - transitioned: true iff this call moved the state to cancel_requested
//     (a terminal or already-cancel-requested task is an idempotent no-op).
func CancelImageTask(ownerUserID int, publicTaskID string, now int64) (object *dto.ImageTaskObject, transitioned bool, err error) {
	won, exec, casErr := model.RequestImageTaskCancelCAS(publicTaskID, ownerUserID, now)
	if casErr != nil {
		if errors.Is(casErr, gorm.ErrRecordNotFound) {
			return nil, false, imageTaskNotFound()
		}
		return nil, false, fmt.Errorf("cancel image task %s for user %d: %w", publicTaskID, ownerUserID, casErr)
	}
	return projectImageTaskObject(exec), won, nil
}

// imageTaskRetryAfter returns the Retry-After hint for a state, or 0 when none
// applies. Only manual_review carries one (§6.1); cancel_requested clears the
// long interval because the cancel is now acknowledged.
func imageTaskRetryAfter(s model.ImageTaskExecutionState) int {
	if s == model.ImageTaskStateManualReview {
		return manualReviewRetryAfterSeconds
	}
	return 0
}

func imageTaskNotFound() *dto.ImageTaskRequestError {
	return &dto.ImageTaskRequestError{
		Code:       dto.ImageTaskErrNotFound,
		StatusCode: 404,
		Message:    "image task not found",
	}
}

// projectImageTaskObject maps the durable execution row onto the §6.1 public
// object. The result locator is populated only on completed; the error body
// only on failed. Other states leave them null per the contract.
func projectImageTaskObject(exec *model.ImageTaskExecution) *dto.ImageTaskObject {
	obj := &dto.ImageTaskObject{
		ID:        exec.PublicTaskID,
		Object:    imageTaskObjectKind,
		Status:    projectImageTaskPublicStatus(exec.State),
		CreatedAt: exec.CreatedAt,
		UpdatedAt: exec.UpdatedAt,
	}
	if exec.State == model.ImageTaskStateCompleted && exec.Result.ContentURL != "" {
		obj.Result = &dto.ImageTaskResultLocator{
			ContentURL: exec.Result.ContentURL,
			MimeType:   exec.Result.MimeType,
			SizeBytes:  exec.Result.SizeBytes,
			SHA256:     exec.Result.SHA256,
			ExpiresAt:  exec.Result.ExpiresAt,
		}
	}
	if exec.State == model.ImageTaskStateFailed {
		obj.Error = &dto.ImageTaskErrorBody{
			Code:    imageTaskFailedCode,
			Message: "image task failed",
		}
	}
	return obj
}

// projectImageTaskPublicStatus maps the nine-state execution lifecycle onto
// the seven public statuses. submission_unknown is internal and folds onto
// in_progress (§6.1); an unrecognized state is surfaced as failed rather than
// leaking an internal name to clients.
func projectImageTaskPublicStatus(s model.ImageTaskExecutionState) dto.ImageTaskPublicStatus {
	switch s {
	case model.ImageTaskStateQueued:
		return dto.ImageTaskStatusQueued
	case model.ImageTaskStateSubmitting, model.ImageTaskStateSubmissionUnknown, model.ImageTaskStatePolling:
		return dto.ImageTaskStatusInProgress
	case model.ImageTaskStateCompleted:
		return dto.ImageTaskStatusCompleted
	case model.ImageTaskStateFailed:
		return dto.ImageTaskStatusFailed
	case model.ImageTaskStateCancelRequested:
		return dto.ImageTaskStatusCancelRequested
	case model.ImageTaskStateCancelled:
		return dto.ImageTaskStatusCancelled
	case model.ImageTaskStateManualReview:
		return dto.ImageTaskStatusManualReview
	default:
		return dto.ImageTaskStatusFailed
	}
}
