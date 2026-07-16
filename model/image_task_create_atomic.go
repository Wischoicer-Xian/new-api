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
//  1. look up the idempotency key WITHOUT a lock — a hit replays/conflicts
//     immediately, before any write or owner lock (replay must not depend on
//     the owner row being lockable);
//  2. lock the owner fence (user row FOR UPDATE on MySQL/PostgreSQL, serial
//     write on SQLite);
//  3. re-look up the key under the fence — a concurrent same-key create that
//     committed while we waited on the fence now replays;
//  4. count non-terminal executions under the fence and enforce the §6.1 cap;
//  5. write Task + image_task_execution and reserve via the tx-aware billing
//     ledger, all sharing this transaction.
//
// Two same-owner requests with different keys converge to exactly one new task
// plus one cap rejection; the same key converges to one task with the second
// request replaying it. This is the concurrency-safe enforcement primitive;
// the read-only service.ImageTaskInFlightStatusOf is explicitly NOT a gate.
func CreateImageTaskAtomic(intent ImageTaskCreateIntent) (ImageTaskCreateOutcome, error) {
	if err := validateImageTaskCreateIntent(intent); err != nil {
		return ImageTaskCreateOutcome{}, err
	}
	outcome := ImageTaskCreateOutcome{}
	err := DB.Transaction(func(tx *gorm.DB) error {
		// 1. First idempotency check, lock-free: replay/conflict before any write.
		if stored, found, err := lookupImageTaskExecutionTx(tx, intent.OwnerUserID, intent.Operation, intent.IdempotencyKey); err != nil {
			return fmt.Errorf("image task create: idempotency lookup: %w", err)
		} else if found {
			return replayOrConflict(tx, stored, intent, &outcome)
		}

		// 2. Owner fence: serialize same-owner creates on the user row.
		var fence User
		if err := lockForUpdate(tx).Select("id").First(&fence, intent.OwnerUserID).Error; err != nil {
			return fmt.Errorf("image task create: lock owner fence: %w", err)
		}

		// 3. Second idempotency check under the fence: a concurrent same-key
		//    create that committed while we waited now replays.
		if stored, found, err := lookupImageTaskExecutionTx(tx, intent.OwnerUserID, intent.Operation, intent.IdempotencyKey); err != nil {
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

// lookupImageTaskExecutionTx returns the existing execution for an idempotency
// namespace (owner, operation, key) within tx, or found=false if none.
func lookupImageTaskExecutionTx(tx *gorm.DB, ownerUserID int, operation, idempotencyKey string) (*ImageTaskExecution, bool, error) {
	stored := &ImageTaskExecution{}
	err := tx.Where("owner_user_id = ? AND operation = ? AND idempotency_key = ?", ownerUserID, operation, idempotencyKey).First(stored).Error
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
