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
// §6.1 create. The caller (a future handler) resolves price into ReserveQuota
// and builds the §7.4 BillingSnapshot; this function owns only the transactional
// create + cap enforcement + reserve, not price resolution, so it is agnostic
// to the final product cap value.
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

// CreateImageTaskAtomic performs the §6.1 / §7.4 create in ONE transaction:
// idempotent replay/conflict first, then an owner fence (user row FOR UPDATE),
// then in-flight cap enforcement, then Task + image_task_execution write, then
// the reserve ledger stage with the quota deduction — all sharing the same
// transaction handle so cap and reserve cannot diverge under concurrency.
//
// SQLite serializes via its single writer; MySQL/PostgreSQL serialize
// same-owner creates on the user row lock. Two same-owner requests with
// different keys therefore converge to exactly one new task plus one cap
// rejection; the same key converges to one task with the second request
// replaying it.
//
// This is the concurrency-safe enforcement primitive; the read-only
// service.ImageTaskInFlightStatusOf is explicitly NOT a gate.
func CreateImageTaskAtomic(intent ImageTaskCreateIntent) (ImageTaskCreateOutcome, error) {
	if err := validateImageTaskCreateIntent(intent); err != nil {
		return ImageTaskCreateOutcome{}, err
	}
	outcome := ImageTaskCreateOutcome{}
	err := DB.Transaction(func(tx *gorm.DB) error {
		// 1. Owner fence: serialize same-owner creates on the user row. SELECT
		// FOR UPDATE (MySQL/PostgreSQL) or SQLite serial write makes the
		// count + create + reserve region per-owner critical.
		var fence User
		if err := lockForUpdate(tx).Select("id").First(&fence, intent.OwnerUserID).Error; err != nil {
			return fmt.Errorf("image task create: lock owner fence: %w", err)
		}

		// 2. Idempotent replay/conflict BEFORE cap and reserve: a replay never
		// consumes a slot; a conflict never creates.
		stored := &ImageTaskExecution{}
		lookup := tx.Where("owner_user_id = ? AND operation = ? AND idempotency_key = ?",
			intent.OwnerUserID, intent.Operation, intent.IdempotencyKey).First(stored)
		if lookup.Error == nil {
			if stored.RequestHash != intent.RequestHash {
				return ErrImageTaskIdempotencyConflict
			}
			task := &Task{}
			if err := tx.First(task, stored.TaskDBID).Error; err != nil {
				return fmt.Errorf("image task create: load replayed task: %w", err)
			}
			outcome.Task = task
			outcome.Execution = stored
			return nil // commit read-only replay
		}
		if !errors.Is(lookup.Error, gorm.ErrRecordNotFound) {
			return fmt.Errorf("image task create: idempotency lookup: %w", lookup.Error)
		}

		// 3. In-flight cap (§6.1): count non-terminal executions for this owner
		// under the fence, before creating.
		count, err := CountInFlightImageTasksByOwner(tx, intent.OwnerUserID)
		if err != nil {
			return fmt.Errorf("image task create: count in-flight: %w", err)
		}
		if count >= int64(constant.MaxImageTasksPerUser) {
			return ErrImageTaskInFlightCap
		}

		// 4. Task row (provider/billing projection). Platform marks it as an
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

		// 5. image_task_execution (full lifecycle). Key uniqueness is guaranteed
		// under the owner fence + step-2 lookup; a collision here is a defect.
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

		// 6. Reserve ledger + quota deduction in THIS transaction (§7.4: billing
		// stage and fund/subscription/token changes share the transaction).
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
	if intent.Now == 0 {
		return errors.New("image task create: now required")
	}
	return nil
}

// reserveImageTaskInTx records the reserve billing stage and deducts the
// reserved quota inside tx, mirroring ApplyBillingStage's pending→applying→
// applied CAS but without its own transaction wrapper so it shares
// CreateImageTaskAtomic's transaction. The conditional UPDATE (quota >= amount)
// prevents a negative balance; insufficient quota fails the whole create.
func reserveImageTaskInTx(tx *gorm.DB, taskDBID int64, intent ImageTaskCreateIntent) error {
	ledger := &TaskBillingLedger{
		TaskDBID:        taskDBID,
		Stage:           TaskBillingReserve,
		OperationKey:    BillingOperationKey(taskDBID, TaskBillingReserve),
		State:           BillingStatePending,
		QuotaAmount:     intent.ReserveQuota,
		BillingSnapshot: intent.BillingSnapshot,
		CreatedAt:       intent.Now,
	}
	if err := tx.Create(ledger).Error; err != nil {
		return fmt.Errorf("reserve ledger create: %w", err)
	}
	applying := tx.Model(&TaskBillingLedger{}).
		Where("id = ? AND state = ?", ledger.ID, BillingStatePending).
		Update("state", BillingStateApplying)
	if applying.Error != nil {
		return fmt.Errorf("reserve ledger claim: %w", applying.Error)
	}
	if applying.RowsAffected != 1 {
		return fmt.Errorf("reserve ledger %d lost pending state", ledger.ID)
	}
	// Conditional quota deduction prevents negative balance. A 0-reserve skips
	// the UPDATE so RowsAffected stays uniform (0 deduction always succeeds).
	if intent.ReserveQuota > 0 {
		deducted := tx.Model(&User{}).
			Where("id = ? AND quota >= ?", intent.OwnerUserID, intent.ReserveQuota).
			Update("quota", gorm.Expr("quota - ?", intent.ReserveQuota))
		if deducted.Error != nil {
			return fmt.Errorf("reserve quota deduction: %w", deducted.Error)
		}
		if deducted.RowsAffected != 1 {
			return ErrImageTaskInsufficientQuota
		}
	}
	applied := tx.Model(&TaskBillingLedger{}).
		Where("id = ? AND state = ?", ledger.ID, BillingStateApplying).
		Update("state", BillingStateApplied)
	if applied.Error != nil {
		return fmt.Errorf("reserve ledger finalize: %w", applied.Error)
	}
	if applied.RowsAffected != 1 {
		return fmt.Errorf("reserve ledger %d lost applying state", ledger.ID)
	}
	return nil
}

// GenerateImageTaskPublicID returns a new imgtask_ public id.
func GenerateImageTaskPublicID() string {
	key, _ := common.GenerateRandomCharsKey(24)
	return "imgtask_" + key
}
