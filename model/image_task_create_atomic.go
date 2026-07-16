package model

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"gorm.io/gorm"
)

// Image task create errors. A future handler maps these to the §6.1 HTTP
// responses (409 IDEMPOTENCY_CONFLICT / 429 TOO_MANY_REQUESTS / insufficient
// quota).
var (
	ErrImageTaskIdempotencyConflict = errors.New("idempotency key reused with a different request")
	ErrImageTaskInFlightCap         = errors.New("per-user in-flight image task cap reached")
	ErrImageTaskInsufficientQuota   = errors.New("insufficient quota to reserve image task")
)

// ImageTaskCreateIntent captures everything CreateImageTaskAtomic needs for one
// §6.1 create. The caller resolves price into ReserveQuota and builds the §7.4
// BillingSnapshot; this function owns the transactional create + cap
// enforcement + reserve. The full funding-source/subscription/token billing
// aggregate (P1-1, grounded in BillingSession) is the next increment; today
// reserve deducts the wallet path inside the create transaction.
type ImageTaskCreateIntent struct {
	OwnerUserID       int
	Group             string
	CreationTokenID   int
	ChannelID         int
	Operation         string // ImageTaskOperationGeneration | ImageTaskOperationEdit
	IdempotencyKey    string
	RequestHash       string
	ChannelRevisionID int64
	ExecutionMode     string
	AdapterVersion    string
	ReserveQuota      int
	BillingSnapshot   json.RawMessage
	Now               int64
}

// ImageTaskCreateOutcome is the result of one atomic create. Created is false
// on an idempotent replay (the existing Task/Execution are returned).
type ImageTaskCreateOutcome struct {
	Task      *Task
	Execution *ImageTaskExecution
	Created   bool
}

// CreateImageTaskAtomic performs the §6.1 / §7.4 create in ONE transaction with
// a double-checked idempotency guard:
//
//  1. a lock-free replay/conflict fast path run OUTSIDE the create transaction;
//  2. inside the transaction, lock the owner fence (user row FOR UPDATE on
//     MySQL/PostgreSQL, serial write on SQLite) — this is the FIRST statement
//     of the transaction, so its snapshot (MySQL REPEATABLE READ) is taken here,
//     after any concurrent same-owner commit;
//  3. re-look up the key under the fence — a concurrent same-key create that
//     committed while we waited now replays;
//  4. count non-terminal executions under the fence and enforce the §6.1 cap;
//  5. write Task + image_task_execution and reserve via the tx-aware billing
//     ledger, all sharing this transaction.
//
// The fast path must stay outside the transaction: a read before the fence
// inside the tx would pin a REPEATABLE READ snapshot and hide a concurrent
// same-key/same-owner commit from the authoritative in-tx check, causing
// duplicate-key inserts and cap under-count on MySQL. Two same-owner requests
// with different keys converge to exactly one new task plus one cap rejection;
// the same key converges to one task with the second request replaying it.
// This is the concurrency-safe enforcement primitive; the read-only
// service.ImageTaskInFlightStatusOf is explicitly NOT a gate.
func CreateImageTaskAtomic(intent ImageTaskCreateIntent) (ImageTaskCreateOutcome, error) {
	if err := validateImageTaskCreateIntent(intent); err != nil {
		return ImageTaskCreateOutcome{}, err
	}

	// 1. Fast path: lock-free replay/conflict OUTSIDE the create transaction.
	if stored, found, err := lookupImageTaskExecution(DB, intent.OwnerUserID, intent.Operation, intent.IdempotencyKey); err != nil {
		return ImageTaskCreateOutcome{}, fmt.Errorf("image task create: fast-path lookup: %w", err)
	} else if found {
		var fast ImageTaskCreateOutcome
		if err := replayOrConflict(DB, stored, intent, &fast); err != nil {
			return ImageTaskCreateOutcome{}, err
		}
		return fast, nil
	}

	outcome := ImageTaskCreateOutcome{}
	err := DB.Transaction(func(tx *gorm.DB) error {
		// 2. Owner fence: the FIRST statement of the transaction, so the snapshot
		// is taken here and includes any same-owner commit that finished while we
		// waited for the row lock.
		var fence User
		if err := lockForUpdate(tx).Select("id").First(&fence, intent.OwnerUserID).Error; err != nil {
			return fmt.Errorf("image task create: lock owner fence: %w", err)
		}

		// 3. Authoritative idempotency check under the fence.
		if stored, found, err := lookupImageTaskExecution(tx, intent.OwnerUserID, intent.Operation, intent.IdempotencyKey); err != nil {
			return fmt.Errorf("image task create: fenced idempotency lookup: %w", err)
		} else if found {
			return replayOrConflict(tx, stored, intent, &outcome)
		}

		// 4. In-flight cap (§6.1) under the fence, before creating.
		count, err := CountInFlightImageTasksByOwner(tx, intent.OwnerUserID)
		if err != nil {
			return fmt.Errorf("image task create: count in-flight: %w", err)
		}
		if count >= int64(constant.MaxImageTasksPerUser) {
			return ErrImageTaskInFlightCap
		}

		// 5. Task row (provider/billing projection). Platform marks it as an
		// image task so the legacy poller excludes it (§7.3).
		task := &Task{
			TaskID:     GenerateTaskID(),
			UserId:     intent.OwnerUserID,
			Group:      intent.Group,
			ChannelId:  intent.ChannelID,
			Platform:   constant.TaskPlatformWischoicerImage,
			Action:     intent.Operation,
			Status:     TaskStatusNotStart,
			Progress:   "0%",
			SubmitTime: intent.Now,
			Quota:      intent.ReserveQuota,
		}
		if err := tx.Create(task).Error; err != nil {
			return fmt.Errorf("image task create: create task: %w", err)
		}

		exec := &ImageTaskExecution{
			PublicTaskID:      GenerateImageTaskPublicID(),
			TaskDBID:          task.ID,
			OwnerUserID:       intent.OwnerUserID,
			CreationTokenID:   intent.CreationTokenID,
			Operation:         intent.Operation,
			IdempotencyKey:    intent.IdempotencyKey,
			RequestHash:       intent.RequestHash,
			State:             ImageTaskStateQueued,
			ChannelRevisionID: intent.ChannelRevisionID,
			ExecutionMode:     intent.ExecutionMode,
			AdapterVersion:    intent.AdapterVersion,
			CreatedAt:         intent.Now,
			UpdatedAt:         intent.Now,
		}
		if err := tx.Create(exec).Error; err != nil {
			return fmt.Errorf("image task create: create execution: %w", err)
		}

		// 6. Reserve via the tx-aware billing ledger, sharing this transaction
		// (§7.4): billing stage and fund changes commit or roll back together.
		if err := reserveImageTaskInTx(tx, task.ID, intent); err != nil {
			return err
		}

		outcome.Task = task
		outcome.Execution = exec
		outcome.Created = true
		return nil
	})
	if err != nil {
		return ImageTaskCreateOutcome{}, err
	}
	return outcome, nil
}

// lookupImageTaskExecution returns the existing execution for an idempotency
// namespace (owner, operation, key) via db (model.DB or a tx handle), or
// found=false if none.
func lookupImageTaskExecution(db *gorm.DB, ownerUserID int, operation, idempotencyKey string) (*ImageTaskExecution, bool, error) {
	stored := &ImageTaskExecution{}
	err := db.Where("owner_user_id = ? AND operation = ? AND idempotency_key = ?", ownerUserID, operation, idempotencyKey).First(stored).Error
	if err == nil {
		return stored, true, nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, nil
	}
	return nil, false, err
}

// replayOrConflict resolves a found idempotency key: matching hash replays the
// stored task (no slot consumed, no reserve); a mismatch is a conflict.
func replayOrConflict(tx *gorm.DB, stored *ImageTaskExecution, intent ImageTaskCreateIntent, outcome *ImageTaskCreateOutcome) error {
	if stored.RequestHash != intent.RequestHash {
		return ErrImageTaskIdempotencyConflict
	}
	task := &Task{}
	if err := tx.First(task, stored.TaskDBID).Error; err != nil {
		return fmt.Errorf("image task create: load replayed task: %w", err)
	}
	outcome.Task = task
	outcome.Execution = stored
	return nil
}

func validateImageTaskCreateIntent(intent ImageTaskCreateIntent) error {
	if intent.OwnerUserID <= 0 {
		return errors.New("image task create: owner_user_id required")
	}
	if intent.IdempotencyKey == "" {
		return errors.New("image task create: idempotency_key required")
	}
	if intent.RequestHash == "" {
		return errors.New("image task create: request_hash required")
	}
	if intent.Operation != ImageTaskOperationGeneration && intent.Operation != ImageTaskOperationEdit {
		return fmt.Errorf("image task create: invalid operation %q", intent.Operation)
	}
	if intent.ReserveQuota < 0 {
		return errors.New("image task create: reserve quota must not be negative")
	}
	if len(intent.BillingSnapshot) == 0 {
		return errors.New("image task create: billing_snapshot required")
	}
	if intent.Now == 0 {
		return errors.New("image task create: now required")
	}
	return nil
}

// reserveImageTaskInTx records the reserve billing stage and applies it inside
// tx, reusing the tx-aware billing-ledger state machine (RecordBillingStage +
// ApplyBillingStageTx) so the §7.4 snapshot/MaxQuota validation and the
// pending→applying→applied CAS are shared with the rest of the codebase rather
// than hand-rolled. The apply callback performs the tx-aware fund deduction;
// today that is the wallet path (conditional UPDATE prevents a negative
// balance). The funding-source/subscription/token aggregate (P1-1) replaces
// this callback next.
func reserveImageTaskInTx(tx *gorm.DB, taskDBID int64, intent ImageTaskCreateIntent) error {
	_, ledger, err := RecordBillingStage(tx, TaskBillingStageIntent{
		TaskDBID:    taskDBID,
		Stage:       TaskBillingReserve,
		Snapshot:    intent.BillingSnapshot,
		QuotaAmount: intent.ReserveQuota,
	})
	if err != nil {
		return fmt.Errorf("reserve ledger record: %w", err)
	}
	if _, err := ApplyBillingStageTx(tx, ledger.ID, func(t *gorm.DB, _ *TaskBillingLedger) error {
		return deductWalletInTx(t, intent.OwnerUserID, intent.ReserveQuota)
	}); err != nil {
		return err
	}
	return nil
}

// deductWalletInTx performs a tx-aware, conditional wallet deduction that
// prevents a negative balance. It is the tx-aware analogue of the wallet
// funding path (model.DecreaseUserQuota minus the async cache + batch): no
// global DB, no self-opened transaction, no fire-and-forget cache mutation.
// Redis quota cache convergence is intentionally NOT done here; it must happen
// after commit (P1-1 funding aggregate), so a rollback never dirties the cache.
func deductWalletInTx(tx *gorm.DB, ownerUserID, amount int) error {
	if amount <= 0 {
		return nil
	}
	deducted := tx.Model(&User{}).
		Where("id = ? AND quota >= ?", ownerUserID, amount).
		Update("quota", gorm.Expr("quota - ?", amount))
	if deducted.Error != nil {
		return fmt.Errorf("reserve quota deduction: %w", deducted.Error)
	}
	if deducted.RowsAffected != 1 {
		return ErrImageTaskInsufficientQuota
	}
	return nil
}

// GenerateImageTaskPublicID returns a new imgtask_ public id.
func GenerateImageTaskPublicID() string {
	key, _ := common.GenerateRandomCharsKey(24)
	return "imgtask_" + key
}
