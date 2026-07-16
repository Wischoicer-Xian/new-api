package model

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// TaskBillingStage names one of the durable billing transitions for a task.
// The ledger records each stage at most once per task, making reserve/settle/
// refund idempotent across crashes and replays.
type TaskBillingStage string

const (
	TaskBillingReserve TaskBillingStage = "reserve"
	TaskBillingSettle  TaskBillingStage = "settle"
	TaskBillingRefund  TaskBillingStage = "refund"
)

var (
	ErrInvalidBillingStage = errors.New("invalid task billing stage")
	ErrNegativeQuotaAmount = errors.New("task billing quota amount must not be negative")
)

// Billing ledger row states.
const (
	BillingStatePending      = "pending"
	BillingStateApplying     = "applying"
	BillingStateApplied      = "applied"
	BillingStateFailed       = "failed"
	BillingStateManualReview = "manual_review"
)

// TaskBillingLedger is the durable record that a billing stage was applied to
// a task. The unique (task, stage) pair is the idempotency boundary: no matter
// how many times a worker replays reserve/settle/refund after a crash, each
// stage's side effect is applied at most once. BillingSnapshot freezes the
// price, funding source and attribution captured at submit time so settlement
// does not depend on later config changes.
type TaskBillingLedger struct {
	ID           int64            `json:"id" gorm:"primary_key;AUTO_INCREMENT"`
	TaskDBID     int64            `json:"task_db_id" gorm:"index;uniqueIndex:idx_task_billing_stage,priority:1"`
	Stage        TaskBillingStage `json:"stage" gorm:"type:varchar(20);uniqueIndex:idx_task_billing_stage,priority:2"`
	OperationKey string           `json:"operation_key" gorm:"type:varchar(255);uniqueIndex"`
	State        string           `json:"state" gorm:"type:varchar(20);index"`

	QuotaAmount     int             `json:"quota_amount"`
	BillingSnapshot json.RawMessage `json:"billing_snapshot" gorm:"type:json"`

	AttemptCount int    `json:"attempt_count"`
	NextRunAt    int64  `json:"next_run_at" gorm:"index"`
	LastError    string `json:"last_error" gorm:"type:varchar(255)"`

	CreatedAt int64 `json:"created_at" gorm:"index"`
	UpdatedAt int64 `json:"updated_at"`
}

// BillingOperationKey builds the stable key identifying one billing stage of
// one task. It is the value stored in OperationKey and the conceptual unit of
// idempotency: the same key applied twice must affect the ledger once.
func BillingOperationKey(taskDBID int64, stage TaskBillingStage) string {
	return fmt.Sprintf("billing:%d:%s", taskDBID, stage)
}

type TaskBillingStageIntent struct {
	TaskDBID    int64
	Stage       TaskBillingStage
	Snapshot    json.RawMessage
	QuotaAmount int
}

// RecordBillingStage inserts a ledger row for (task, stage) if none exists and
// returns the stored row along with whether this call created it. A replay of
// a stage that was already recorded returns the existing row without
// re-applying, which is what keeps reserve/settle/refund exactly-once across
// crashes. ApplyBillingStage performs the actual quota mutation in the same
// transaction as the pending-to-applied state transition.
func RecordBillingStage(tx *gorm.DB, intent TaskBillingStageIntent) (created bool, ledger *TaskBillingLedger, err error) {
	if tx == nil {
		return false, nil, fmt.Errorf("record billing stage: nil database transaction")
	}
	if intent.Stage != TaskBillingReserve && intent.Stage != TaskBillingSettle && intent.Stage != TaskBillingRefund {
		return false, nil, ErrInvalidBillingStage
	}
	if intent.QuotaAmount < 0 {
		return false, nil, ErrNegativeQuotaAmount
	}
	if intent.QuotaAmount > common.MaxQuota {
		return false, nil, fmt.Errorf("record billing stage: quota amount %d exceeds maximum %d", intent.QuotaAmount, common.MaxQuota)
	}
	if len(intent.Snapshot) != 0 {
		var decoded any
		if err := common.Unmarshal(intent.Snapshot, &decoded); err != nil {
			return false, nil, fmt.Errorf("record billing stage: invalid snapshot JSON: %w", err)
		}
	}
	ledger = &TaskBillingLedger{
		TaskDBID:        intent.TaskDBID,
		Stage:           intent.Stage,
		OperationKey:    BillingOperationKey(intent.TaskDBID, intent.Stage),
		State:           BillingStatePending,
		QuotaAmount:     intent.QuotaAmount,
		BillingSnapshot: intent.Snapshot,
	}
	result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(ledger)
	if result.Error != nil {
		return false, nil, result.Error
	}
	if result.RowsAffected == 1 {
		return true, ledger, nil
	}
	stored := &TaskBillingLedger{}
	if err := tx.Where("task_db_id = ? AND stage = ?", intent.TaskDBID, intent.Stage).First(stored).Error; err != nil {
		return false, nil, err
	}
	return false, stored, nil
}

// ApplyBillingStage serializes one pending ledger row, runs every associated
// quota/subscription/token mutation, and flips the ledger to applied in the
// same transaction. The callback must perform all writes through tx. Replays
// of an already-applied stage return won=false without invoking the callback.
func ApplyBillingStage(db *gorm.DB, id int64, apply func(tx *gorm.DB, ledger *TaskBillingLedger) error) (won bool, err error) {
	if db == nil {
		return false, fmt.Errorf("apply billing stage: nil database")
	}
	if apply == nil {
		return false, fmt.Errorf("apply billing stage: nil callback")
	}
	// ApplyBillingStage is the self-managed-transaction wrapper; the CAS itself
	// lives in ApplyBillingStageTx so there is exactly one pending→applying→
	// applied state machine in the codebase.
	err = db.Transaction(func(tx *gorm.DB) error {
		w, e := ApplyBillingStageTx(tx, id, apply)
		won = w
		return e
	})
	if err != nil {
		return false, err
	}
	return won, nil
}

// ApplyBillingStageTx is the tx-aware variant of ApplyBillingStage: it runs the
// pending→applying→applied CAS inside the caller-supplied transaction instead
// of opening its own. Use it when the stage must commit or roll back with a
// surrounding write (e.g. an image-task create that reserves quota and writes
// the ledger in one transaction, §7.4). The CAS, state checks, and error
// semantics mirror ApplyBillingStage exactly; only the transaction boundary
// differs (caller decides).
func ApplyBillingStageTx(tx *gorm.DB, id int64, apply func(tx *gorm.DB, ledger *TaskBillingLedger) error) (won bool, err error) {
	if tx == nil {
		return false, fmt.Errorf("apply billing stage tx: nil transaction")
	}
	if apply == nil {
		return false, fmt.Errorf("apply billing stage tx: nil callback")
	}
	claim := tx.Model(&TaskBillingLedger{}).
		Where("id = ? AND state = ?", id, BillingStatePending).
		Update("state", BillingStateApplying)
	if claim.Error != nil {
		return false, claim.Error
	}
	if claim.RowsAffected != 1 {
		var state string
		if err := tx.Model(&TaskBillingLedger{}).Select("state").Where("id = ?", id).Scan(&state).Error; err != nil {
			return false, err
		}
		if state == BillingStateApplied {
			return false, nil
		}
		if state == "" {
			return false, gorm.ErrRecordNotFound
		}
		return false, fmt.Errorf("apply billing stage tx: ledger %d is in state %q", id, state)
	}
	ledger := &TaskBillingLedger{}
	if err := tx.First(ledger, id).Error; err != nil {
		return false, err
	}
	if err := apply(tx, ledger); err != nil {
		return false, err
	}
	result := tx.Model(&TaskBillingLedger{}).
		Where("id = ? AND state = ?", id, BillingStateApplying).
		Update("state", BillingStateApplied)
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected != 1 {
		return false, fmt.Errorf("apply billing stage tx: ledger %d lost applying state", id)
	}
	return true, nil
}
