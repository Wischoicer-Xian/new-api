package model

import (
	"database/sql/driver"
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Image task operation, mirrored from the service-layer capability types.
// Kept as plain strings here so the model package does not import service.
const (
	ImageTaskOperationGeneration = "generation"
	ImageTaskOperationEdit       = "edit"
)

// ImageTaskExecutionState is the rich lifecycle of one single-image task.
// Only these values are stored on the extension row; the legacy Task.Status
// column keeps its existing values and is driven as a compatibility
// projection so old video polling and admin views are unaffected.
type ImageTaskExecutionState string

const (
	ImageTaskStateQueued            ImageTaskExecutionState = "queued"
	ImageTaskStateSubmitting        ImageTaskExecutionState = "submitting"
	ImageTaskStateSubmissionUnknown ImageTaskExecutionState = "submission_unknown"
	ImageTaskStatePolling           ImageTaskExecutionState = "polling"
	ImageTaskStateCompleted         ImageTaskExecutionState = "completed"
	ImageTaskStateFailed            ImageTaskExecutionState = "failed"
	ImageTaskStateCancelRequested   ImageTaskExecutionState = "cancel_requested"
	ImageTaskStateCancelled         ImageTaskExecutionState = "cancelled"
	ImageTaskStateManualReview      ImageTaskExecutionState = "manual_review"
)

// terminalImageTaskStates is the single source of truth for which execution
// states are terminal. Both IsTerminalImageTaskState and the revision
// deletion gate derive from it so a state change updates one place.
var terminalImageTaskStates = []ImageTaskExecutionState{
	ImageTaskStateCompleted,
	ImageTaskStateFailed,
	ImageTaskStateCancelled,
}

// terminalImageTaskStateStrings is the string form for use in NOT IN clauses
// where GORM needs []string rather than the typed slice.
var terminalImageTaskStateStrings = func() []string {
	out := make([]string, len(terminalImageTaskStates))
	for i, s := range terminalImageTaskStates {
		out[i] = string(s)
	}
	return out
}()

// IsTerminalImageTaskState reports whether a state is terminal: no further
// worker transition is expected. The polling/submitting states are
// non-terminal even under cancel, because a submitted remote execution must
// still be drained.
func IsTerminalImageTaskState(s ImageTaskExecutionState) bool {
	for _, t := range terminalImageTaskStates {
		if s == t {
			return true
		}
	}
	return false
}

// ImageTaskResult is the durable single-image result locator persisted once
// the provider returns a usable image. Stored as JSON so adding fields does
// not require a migration.
type ImageTaskResult struct {
	ContentURL string `json:"content_url,omitempty"`
	MimeType   string `json:"mime_type,omitempty"`
	SizeBytes  int64  `json:"size_bytes,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
	ExpiresAt  int64  `json:"expires_at,omitempty"`
}

func (r *ImageTaskResult) Scan(val interface{}) error {
	bytesValue, _ := val.([]byte)
	if len(bytesValue) == 0 {
		*r = ImageTaskResult{}
		return nil
	}
	return common.Unmarshal(bytesValue, r)
}

func (r ImageTaskResult) Value() (driver.Value, error) {
	if r == (ImageTaskResult{}) {
		return nil, nil
	}
	return common.Marshal(r)
}

// ImageTaskExecution is the one-to-one extension of an existing Task row for
// the public single-image task API. The existing Task remains the provider /
// billing compatibility projection; this row owns the full image-task
// lifecycle, the idempotency namespace, and the durable lease used by the
// image processor.
type ImageTaskExecution struct {
	ID int64 `json:"id" gorm:"primary_key;AUTO_INCREMENT"`
	// PublicTaskID is the imgtask_* id exposed to callers; unique per task.
	PublicTaskID string `json:"public_task_id" gorm:"type:varchar(64);uniqueIndex"`
	// TaskDBID references the existing Task.ID that carries provider billing.
	TaskDBID int64 `json:"task_db_id" gorm:"uniqueIndex"`

	OwnerUserID     int    `json:"owner_user_id" gorm:"index;uniqueIndex:idx_image_task_idem,priority:1"`
	CreationTokenID int    `json:"creation_token_id" gorm:"index"`
	Operation       string `json:"operation" gorm:"type:varchar(20);uniqueIndex:idx_image_task_idem,priority:2"`
	IdempotencyKey  string `json:"idempotency_key" gorm:"type:varchar(191);uniqueIndex:idx_image_task_idem,priority:3"`
	RequestHash     string `json:"request_hash" gorm:"type:varchar(64);index"`

	State              ImageTaskExecutionState `json:"state" gorm:"type:varchar(30);index"`
	SubmissionState    string                  `json:"submission_state" gorm:"type:varchar(30)"`
	ClientSubmissionID string                  `json:"client_submission_id" gorm:"type:varchar(191)"`

	ChannelRevisionID int64  `json:"channel_revision_id" gorm:"index"`
	ExecutionMode     string `json:"execution_mode" gorm:"type:varchar(20)"`
	AdapterVersion    string `json:"adapter_version" gorm:"type:varchar(64)"`

	NextRunAt       int64  `json:"next_run_at" gorm:"index"`
	LeaseOwner      string `json:"lease_owner" gorm:"type:varchar(64)"`
	LeaseUntil      int64  `json:"lease_until" gorm:"index"`
	LeaseGeneration int    `json:"lease_generation"`

	PollCount        int `json:"poll_count"`
	SubmitErrorCount int `json:"submit_error_count"`
	PollErrorCount   int `json:"poll_error_count"`
	ResultErrorCount int `json:"result_error_count"`

	CancelRequestedAt int64 `json:"cancel_requested_at"`

	Result             ImageTaskResult `json:"result" gorm:"type:json"`
	ManualReviewReason string          `json:"manual_review_reason" gorm:"type:varchar(255)"`

	CreatedAt  int64 `json:"created_at" gorm:"index"`
	UpdatedAt  int64 `json:"updated_at"`
	FinishedAt int64 `json:"finished_at" gorm:"index"`
}

// ErrIdempotencyConflict is returned when an idempotency key is reused with
// a different canonical request hash.
var ErrIdempotencyConflict = errors.New("idempotency key reused with a different request")

// FindImageTaskReplay performs the lock-free replay preflight used before
// mutable channel and pricing resolution. The reserve transaction remains the
// authoritative convergence point for concurrent creates.
func FindImageTaskReplay(ownerUserID int, operation, idempotencyKey, requestHash string) (*ImageTaskExecution, *Task, error) {
	var exec ImageTaskExecution
	err := DB.Where("owner_user_id = ? AND operation = ? AND idempotency_key = ?", ownerUserID, operation, idempotencyKey).First(&exec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if exec.RequestHash != requestHash {
		return nil, nil, ErrIdempotencyConflict
	}
	var task Task
	if err := DB.First(&task, exec.TaskDBID).Error; err != nil {
		return nil, nil, err
	}
	return &exec, &task, nil
}

// CreateOrGetImageTaskExecution inserts a new execution, or — when the
// idempotency namespace (owner, operation, key) already exists — returns the
// stored row. The unique index idx_image_task_idem is the convergence point
// for concurrent identical requests: whatever the interleaving, at most one
// row is created per key. A key reused with a different request hash is a
// conflict, not a replay.
func CreateOrGetImageTaskExecution(tx *gorm.DB, exec *ImageTaskExecution) (created bool, existing *ImageTaskExecution, err error) {
	if tx == nil {
		return false, nil, fmt.Errorf("create image task execution: nil database transaction")
	}
	stored := &ImageTaskExecution{}
	lookup := tx.Where(
		"owner_user_id = ? AND operation = ? AND idempotency_key = ?",
		exec.OwnerUserID, exec.Operation, exec.IdempotencyKey,
	).First(stored)
	if lookup.Error == nil {
		if stored.RequestHash != exec.RequestHash {
			return false, stored, ErrIdempotencyConflict
		}
		return false, stored, nil
	}
	if !errors.Is(lookup.Error, gorm.ErrRecordNotFound) {
		return false, nil, lookup.Error
	}

	var revisionCandidate ChannelRevision
	if exec.ChannelRevisionID != 0 {
		if err := tx.Select("id", "channel_id").First(&revisionCandidate, exec.ChannelRevisionID).Error; err != nil {
			return false, nil, fmt.Errorf("find channel revision: %w", err)
		}
	}
	err = tx.Transaction(func(createTx *gorm.DB) error {
		if exec.ChannelRevisionID != 0 {
			if err := lockImageChannelRevisionFence(createTx, revisionCandidate.ChannelID); err != nil {
				return fmt.Errorf("lock channel revision fence: %w", err)
			}
			var revision ChannelRevision
			if err := lockForUpdate(createTx).Where("channel_id = ?", revisionCandidate.ChannelID).Select("id").First(&revision, exec.ChannelRevisionID).Error; err != nil {
				return fmt.Errorf("lock channel revision: %w", err)
			}
		}
		result := createTx.Clauses(clause.OnConflict{DoNothing: true}).Create(exec)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 1 {
			created = true
			existing = exec
			return nil
		}
		// A unique constraint won the race. ON CONFLICT DO NOTHING is required here:
		// a plain duplicate-key error aborts PostgreSQL transactions and would make
		// this convergence SELECT impossible inside the task-creation transaction.
		stored := &ImageTaskExecution{}
		if err := createTx.Where(
			"owner_user_id = ? AND operation = ? AND idempotency_key = ?",
			exec.OwnerUserID, exec.Operation, exec.IdempotencyKey,
		).First(stored).Error; err != nil {
			return err
		}
		existing = stored
		if stored.RequestHash != exec.RequestHash {
			return ErrIdempotencyConflict
		}
		return nil
	})
	if err != nil {
		return false, existing, err
	}
	return created, existing, nil
}

type ImageTaskTerminalTransition struct {
	ID                 int64
	LeaseOwner         string
	ExpectedGeneration int
	Now                int64
	From               ImageTaskExecutionState
	To                 ImageTaskExecutionState
}

// FinalizeImageTaskExecutionCAS advances an execution to a terminal state and
// runs every associated durable write in one transaction. A stale worker or a
// loser of the state CAS never invokes finalize. If finalize fails, the state
// transition rolls back with its legacy projection and billing/result writes.
func FinalizeImageTaskExecutionCAS(db *gorm.DB, transition ImageTaskTerminalTransition, finalize func(tx *gorm.DB) error) (won bool, err error) {
	if db == nil {
		return false, fmt.Errorf("mark image task terminal: nil database transaction")
	}
	if finalize == nil {
		return false, fmt.Errorf("mark image task terminal: nil finalize callback")
	}
	if !IsTerminalImageTaskState(transition.To) {
		return false, fmt.Errorf("mark image task terminal: target state %q is not terminal", transition.To)
	}
	if IsTerminalImageTaskState(transition.From) {
		return false, fmt.Errorf("mark image task terminal: source state %q is already terminal", transition.From)
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&ImageTaskExecution{}).
			Where("id = ? AND state = ? AND lease_owner = ? AND lease_generation = ? AND lease_until >= ?", transition.ID, transition.From, transition.LeaseOwner, transition.ExpectedGeneration, transition.Now).
			Update("state", transition.To)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return nil
		}
		if err := finalize(tx); err != nil {
			return err
		}
		won = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return won, nil
}

// GetImageTaskExecutionByPublicTaskID loads an execution by its public
// imgtask_* id, scoped to ownerUserID. A task owned by another user (or a
// missing one) is reported as gorm.ErrRecordNotFound so the GET/cancel
// handlers map it to 404 without leaking existence across accounts (§6.1:
// GET/cancel follow new-api's existing user-level task ownership).
func GetImageTaskExecutionByPublicTaskID(publicTaskID string, ownerUserID int) (*ImageTaskExecution, error) {
	var exec ImageTaskExecution
	err := DB.Where("public_task_id = ? AND owner_user_id = ?", publicTaskID, ownerUserID).First(&exec).Error
	if err != nil {
		return nil, err
	}
	return &exec, nil
}

// RequestImageTaskCancelCAS marks a non-terminal execution as cancel-requested.
// It is the only state transition the public cancel handler performs; the
// actual provider cancellation and terminal settle/refund are the processor's
// job (§9.2 cancel_guard: once cancel_requested, submit/failover and
// next-generation are forbidden; only poll/cancel/reconcile/settle proceed).
//
// A terminal execution (completed/failed/cancelled) or one already in
// cancel_requested is an idempotent no-op: won is false and the current row is
// returned so the handler can report the present state. manual_review remains
// cancellable (§6.1: the state is non-terminal and "裁决前允许取消"), so only
// the terminal set blocks the transition.
//
// The row is locked before the state check so concurrent cancel requests and a
// processor transition serialize on the same fence.
func RequestImageTaskCancelCAS(publicTaskID string, ownerUserID int, now int64) (won bool, exec *ImageTaskExecution, err error) {
	err = DB.Transaction(func(tx *gorm.DB) error {
		var current ImageTaskExecution
		if err := lockForUpdate(tx).Where("public_task_id = ? AND owner_user_id = ?", publicTaskID, ownerUserID).First(&current).Error; err != nil {
			return err
		}
		if IsTerminalImageTaskState(current.State) || current.State == ImageTaskStateCancelRequested || current.CancelRequestedAt != 0 {
			exec = &current
			return nil
		}
		if current.State == ImageTaskStateSubmitting {
			result := tx.Model(&ImageTaskExecution{}).
				Where("id = ? AND state = ? AND cancel_requested_at = 0", current.ID, current.State).
				Updates(map[string]any{
					"cancel_requested_at": now,
					"updated_at":          now,
				})
			if result.Error != nil {
				return result.Error
			}
			won = result.RowsAffected == 1
			current.CancelRequestedAt = now
			current.UpdatedAt = now
			exec = &current
			return nil
		}
		result := tx.Model(&ImageTaskExecution{}).
			Where("id = ? AND state = ?", current.ID, current.State).
			Updates(map[string]any{
				"state":               string(ImageTaskStateCancelRequested),
				"cancel_requested_at": now,
				"updated_at":          now,
			})
		if result.Error != nil {
			return result.Error
		}
		won = result.RowsAffected == 1
		if won {
			current.State = ImageTaskStateCancelRequested
			current.CancelRequestedAt = now
			current.UpdatedAt = now
			exec = &current
			return nil
		}
		if err := tx.First(&current, current.ID).Error; err != nil {
			return err
		}
		exec = &current
		return nil
	})
	return won, exec, err
}
