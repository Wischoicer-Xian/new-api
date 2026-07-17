package model

import (
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// ImageTaskAdvance is the fenced non-terminal transition command. The CAS
// validates that this lease owner still holds the execution at the expected
// generation and that the from_state matches, so a stale worker or a lease
// loser never mutates the row. The processor uses it for every non-terminal
// step (queued→submitting, submitting→polling, backoff, result-store pending,
// manual_review routing); terminal steps go through FinalizeImageTask, which
// owns the billing settle/refund in the same fenced transaction.
type ImageTaskAdvance struct {
	ID                 int64
	LeaseOwner         string
	ExpectedGeneration int
	Now                int64
	From               ImageTaskExecutionState
	To                 ImageTaskExecutionState
}

// AdvanceImageTaskExecutionCAS performs a fenced non-terminal state transition
// (or a same-state field update when From == To) and runs the caller's side
// writes inside the same transaction. The fence is a conditional UPDATE keyed on
// id + from_state + lease_owner + lease_generation + lease_until >= now; only the
// lease holder at the expected generation can win. updated_at is always bumped so
// a same-state advance still reports RowsAffected=1 on MySQL.
//
// The callback MUST NOT set state (the fence owns the state column); it sets the
// adjacent fields the step needs (client_submission_id, next_run_at, poll/error
// counts, result locator, manual_review_reason). Terminal targets are rejected:
// use FinalizeImageTask so the billing aggregate settles/refunds in one tx.
func AdvanceImageTaskExecutionCAS(adv ImageTaskAdvance, apply func(tx *gorm.DB) error) (won bool, err error) {
	if adv.LeaseOwner == "" {
		return false, fmt.Errorf("advance image task execution: empty lease owner")
	}
	if IsTerminalImageTaskState(adv.To) {
		return false, fmt.Errorf("advance image task execution: target state %q is terminal; use FinalizeImageTask", adv.To)
	}
	if IsTerminalImageTaskState(adv.From) {
		return false, fmt.Errorf("advance image task execution: source state %q is already terminal", adv.From)
	}
	err = DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&ImageTaskExecution{}).
			Where("id = ? AND state = ? AND lease_owner = ? AND lease_generation = ? AND lease_until >= ?",
				adv.ID, adv.From, adv.LeaseOwner, adv.ExpectedGeneration, adv.Now).
			Updates(map[string]any{
				"state":      string(adv.To),
				"updated_at": adv.Now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return nil // CAS lost: stale generation, expired lease, or raced state.
		}
		won = true
		if apply != nil {
			return apply(tx)
		}
		return nil
	})
	return won, err
}

// RecordImageTaskSubmitAcceptedCAS records the upstream handle after the
// provider accepted a submit. A concurrent cancel request during the HTTP call
// is represented by cancel_requested_at while the row remains submitting; this
// transition therefore chooses cancel_requested instead of polling when that
// flag is present. The lease fence prevents a stale submitter from attaching an
// upstream task to a row claimed by another worker.
func RecordImageTaskSubmitAcceptedCAS(adv ImageTaskAdvance, upstreamTaskID string, nextRunAt int64) (won bool, cancelled bool, err error) {
	if adv.LeaseOwner == "" {
		return false, false, errors.New("record image task submit: empty lease owner")
	}
	if upstreamTaskID == "" {
		return false, false, errors.New("record image task submit: empty upstream task id")
	}
	err = DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&ImageTaskExecution{}).
			Where("id = ? AND state = ? AND lease_owner = ? AND lease_generation = ? AND lease_until >= ?",
				adv.ID, ImageTaskStateSubmitting, adv.LeaseOwner, adv.ExpectedGeneration, adv.Now).
			Updates(map[string]any{
				"state": gorm.Expr("CASE WHEN cancel_requested_at > 0 THEN ? ELSE ? END",
					ImageTaskStateCancelRequested, ImageTaskStatePolling),
				"client_submission_id": upstreamTaskID,
				"submission_state":     "accepted",
				"next_run_at":          nextRunAt,
				"updated_at":           adv.Now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return nil
		}
		var current ImageTaskExecution
		if err := tx.Select("state").First(&current, adv.ID).Error; err != nil {
			return err
		}
		won = true
		cancelled = current.State == ImageTaskStateCancelRequested
		return nil
	})
	return won, cancelled, err
}

// RequeueImageTaskSubmitCAS records an explicit provider rejection such as 429.
// Because the provider did not accept the task, a cancel request that arrived
// during the HTTP call wins and moves directly to cancel_requested; otherwise
// the execution returns to queued for a bounded retry.
func RequeueImageTaskSubmitCAS(adv ImageTaskAdvance, submitErrorCount int, nextRunAt int64) (won bool, cancelled bool, err error) {
	err = DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&ImageTaskExecution{}).
			Where("id = ? AND state = ? AND lease_owner = ? AND lease_generation = ? AND lease_until >= ?",
				adv.ID, ImageTaskStateSubmitting, adv.LeaseOwner, adv.ExpectedGeneration, adv.Now).
			Updates(map[string]any{
				"state": gorm.Expr("CASE WHEN cancel_requested_at > 0 THEN ? ELSE ? END",
					ImageTaskStateCancelRequested, ImageTaskStateQueued),
				"submit_error_count": submitErrorCount,
				"submission_state":   "rate_limited",
				"next_run_at":        nextRunAt,
				"updated_at":         adv.Now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return nil
		}
		var current ImageTaskExecution
		if err := tx.Select("state").First(&current, adv.ID).Error; err != nil {
			return err
		}
		won = true
		cancelled = current.State == ImageTaskStateCancelRequested
		return nil
	})
	return won, cancelled, err
}

// ListDueImageTaskExecutions returns claimable executions whose next_run_at has
// arrived and whose lease is free or expired, in due-order (next_run_at, id).
// It is the candidate feed for the processor's bounded due-work pass; the caller
// then applies per-user fairness and TryClaimImageTaskExecution atomically. The
// claimable set excludes terminal states and manual_review (§7.5: manual_review
// waits for an operator and must not be auto-processed).
func ListDueImageTaskExecutions(now int64, limit int) ([]ImageTaskExecution, error) {
	if limit <= 0 {
		limit = 100
	}
	var execs []ImageTaskExecution
	err := DB.Where("next_run_at <= ? AND (lease_until = 0 OR lease_until < ?) AND state IN ?", now, now, claimableImageTaskStates).
		Order("next_run_at ASC, id ASC").Limit(limit).Find(&execs).Error
	return execs, err
}

// HasDueImageTaskExecutions reports whether any execution is due and claimable.
// It backs the image_task_processor system task's Enabled() gate so an idle or
// gated-off system schedules no rows: the scheduler only creates a task row when
// there is work to do, and the handler re-checks the processor gate at run time.
func HasDueImageTaskExecutions() bool {
	now := common.GetTimestamp()
	var count int64
	if err := DB.Model(&ImageTaskExecution{}).
		Where("next_run_at <= ? AND (lease_until = 0 OR lease_until < ?) AND state IN ?", now, now, claimableImageTaskStates).
		Limit(1).Count(&count).Error; err != nil {
		return false
	}
	return count > 0
}

// HasNonTerminalImageTaskExecutions reports whether any image task execution is
// in a non-terminal state. It backs the §14.1 second readiness gate: when
// in-flight work exists, the processor must stay on so those executions keep
// draining (accept/create => read && processor, and new-api read GET is always
// on, so the gate reduces to processor).
//
// Every non-terminal state counts, including manual_review (awaiting an
// operator ruling) and submission_unknown (awaiting reconcile), because each is
// still owed a worker transition; only completed/failed/cancelled are terminal.
// terminalImageTaskStateStrings is reused so the terminal set stays the single
// source of truth defined next to the state constants.
func HasNonTerminalImageTaskExecutions() (bool, error) {
	var count int64
	if err := DB.Model(&ImageTaskExecution{}).
		Where("state NOT IN ?", terminalImageTaskStateStrings).
		Limit(1).Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// MarkImageTaskManualReviewCAS is the fenced transition into manual_review. It is
// the terminal-ish outcome for an execution the processor cannot safely advance
// (submission_unknown past the reconcile SLA, a poll 4xx, an exhausted budget).
// manual_review is non-terminal in the schema (an operator can still cancel or
// rule it), so this uses the advance fence rather than FinalizeImageTask; no
// billing changes here — the operator's ruling drives the later finalize.
func MarkImageTaskManualReviewCAS(adv ImageTaskAdvance, reason string) (won bool, err error) {
	adv.To = ImageTaskStateManualReview
	return AdvanceImageTaskExecutionCAS(adv, func(tx *gorm.DB) error {
		return tx.Model(&ImageTaskExecution{}).Where("id = ?", adv.ID).
			Updates(map[string]any{
				"manual_review_reason": reason,
				"next_run_at":          0, // no further auto-processing until ruled
			}).Error
	})
}

// GetImageTaskReserveQuotaAmount reads the applied reserve quota from the
// execution's reserve ledger. The processor uses it to compute the full-refund
// settle amount on a failed/cancelled terminal (SettleAmount = -reserve). On
// completed the reserve stands as the final charge (SettleAmount = 0). A missing
// reserve ledger is an error: finalize must not run without the durable billing
// truth (§5.7 reads the same ledger inside the CAS callback).
func GetImageTaskReserveQuotaAmount(taskDBID int64) (int, error) {
	var ledger TaskBillingLedger
	err := DB.Where("task_db_id = ? AND stage = ?", taskDBID, TaskBillingReserve).
		Order("id").First(&ledger).Error
	if err != nil {
		return 0, err
	}
	return ledger.QuotaAmount, nil
}

// GetChannelRevisionByID loads a revision by its primary key. The image task
// processor uses this — NOT GetLatestChannelRevisionByChannelID — so a frozen
// task always runs against the exact endpoint/credential it was created under
// (§7.2: a later channel edit must not change an in-flight task's provider call).
func GetChannelRevisionByID(revisionID int64) (*ChannelRevision, error) {
	var rev ChannelRevision
	if err := DB.First(&rev, revisionID).Error; err != nil {
		return nil, err
	}
	return &rev, nil
}

// GetImageTaskExecutionTask loads the legacy Task row backing an execution. The
// processor reads Task.Data to rebuild the provider request from the frozen
// request bytes.
func GetImageTaskExecutionTask(exec *ImageTaskExecution) (*Task, error) {
	if exec == nil {
		return nil, errors.New("nil execution")
	}
	var task Task
	if err := DB.First(&task, exec.TaskDBID).Error; err != nil {
		return nil, err
	}
	return &task, nil
}
