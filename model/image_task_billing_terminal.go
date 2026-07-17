package model

import (
	"context"
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// ImageTaskFinalizeCommand is the immutable command for terminal finalize
// (§5.7). It carries only facts the caller resolved that cannot be read from
// the DB: the target execution, the transition (from→to state), the lease
// identity, and the typed settle/refund amount facts.
type ImageTaskFinalizeCommand struct {
	ExecutionID     int64
	FromState       ImageTaskExecutionState
	ToState         ImageTaskExecutionState
	LeaseOwner      string
	LeaseGeneration int
	Now             int64
	// SettleAmount > 0: charge the delta beyond the already-reserved quota.
	// SettleAmount < 0: refund the unused portion. 0: no adjustment.
	SettleAmount int
}

// FinalizeImageTask is the aggregate's terminal entry point (§5.7). It wraps
// FinalizeImageTaskExecutionCAS (the single CAS, no duplicate) and performs
// the settle/refund billing stage inside the CAS callback.
//
// The CAS WHERE clause simultaneously validates:
//   - execution id + from_state
//   - lease_owner + lease_generation (stale worker loses)
//   - lease_until >= now (expired lease loses)
//
// Inside the callback:
// 1. Read the reserve ledger's ImageTaskBillingSnapshotV1 (durable truth).
// 2. RecordBillingStage + ApplyBillingStageTx for settle/refund.
// 3. Write legacy Task projection (status, progress, finish_time).
//
// Stale generation → CAS RowsAffected=0 → won=false, no side effects.
// Already-terminal execution → CAS returns error (FromState is terminal).
// Callback failure → entire transaction rolls back; execution stays retryable.
//
// NOTE: The provider processor scheduling loop is NOT implemented in this RFC
// (§5.7: "本 RFC 只冻结该接口和不变量；provider processor 尚未启用").
// This method is the interface that a future processor will call when a task
// reaches a terminal provider state.
func FinalizeImageTask(ctx context.Context, cmd ImageTaskFinalizeCommand) (won bool, err error) {
	if err := validateFinalizeCommand(cmd); err != nil {
		return false, err
	}

	transition := ImageTaskTerminalTransition{
		ID:                 cmd.ExecutionID,
		LeaseOwner:         cmd.LeaseOwner,
		ExpectedGeneration: cmd.LeaseGeneration,
		Now:                cmd.Now,
		From:               cmd.FromState,
		To:                 cmd.ToState,
	}

	return FinalizeImageTaskExecutionCAS(DB, transition, func(tx *gorm.DB) error {
		return finalizeCallback(tx, cmd)
	})
}

func validateFinalizeCommand(cmd ImageTaskFinalizeCommand) error {
	if cmd.ExecutionID <= 0 {
		return errors.New("finalize image task: execution_id required")
	}
	if cmd.Now <= 0 {
		return errors.New("finalize image task: now required")
	}
	if !IsTerminalImageTaskState(cmd.ToState) {
		return fmt.Errorf("finalize image task: target state %q is not terminal", cmd.ToState)
	}
	if IsTerminalImageTaskState(cmd.FromState) {
		return fmt.Errorf("finalize image task: source state %q is already terminal", cmd.FromState)
	}
	if cmd.SettleAmount < -common.MaxQuota || cmd.SettleAmount > common.MaxQuota {
		return fmt.Errorf("finalize image task: settle amount out of range %d", cmd.SettleAmount)
	}
	return nil
}

// finalizeCallback runs inside the CAS transaction (§5.7 steps 2-6).
func finalizeCallback(tx *gorm.DB, cmd ImageTaskFinalizeCommand) error {
	// Step 2: Read the reserve ledger's snapshot as the durable truth.
	// The execution row has been CAS-locked to the terminal state by the outer
	// FinalizeImageTaskExecutionCAS; we read the reserve ledger to reconstruct
	// the original billing facts.
	var exec ImageTaskExecution
	if err := tx.First(&exec, cmd.ExecutionID).Error; err != nil {
		return fmt.Errorf("finalize: load execution: %w", err)
	}

	// Read the reserve ledger to get the original billing snapshot.
	var ledger TaskBillingLedger
	if err := tx.Where("task_db_id = ? AND stage = ?", exec.TaskDBID, TaskBillingReserve).
		Order("id").First(&ledger).Error; err != nil {
		return fmt.Errorf("finalize: load reserve ledger: %w", err)
	}
	if ledger.State != BillingStateApplied {
		return fmt.Errorf("finalize: reserve ledger %d is not applied (state=%s)", ledger.ID, ledger.State)
	}

	var snap ImageTaskBillingSnapshotV1
	if err := common.Unmarshal(ledger.BillingSnapshot, &snap); err != nil {
		return fmt.Errorf("finalize: decode snapshot: %w", err)
	}
	// Strict V1 validation after decode (P1-4 RFC §5.3: snapshot must be
	// validated after deserialization to prove it was not tampered).
	if err := snap.Validate(); err != nil {
		return fmt.Errorf("finalize: snapshot validation: %w", err)
	}

	// Step 3-4: If settle amount is non-zero, write settle/refund ledger stage
	// AND perform real fund reversal from the snapshot's funding_source.
	if cmd.SettleAmount != 0 {
		stage := TaskBillingSettle
		amount := cmd.SettleAmount
		if amount < 0 {
			stage = TaskBillingRefund
			amount = -amount
		}
		settleSnap, mErr := common.Marshal(snap)
		if mErr != nil {
			return fmt.Errorf("finalize: marshal settle snapshot: %w", mErr)
		}
		_, sLedger, rErr := RecordBillingStage(tx, TaskBillingStageIntent{
			TaskDBID:    exec.TaskDBID,
			Stage:       stage,
			Snapshot:    settleSnap,
			QuotaAmount: amount,
		})
		if rErr != nil {
			return fmt.Errorf("finalize: record settle stage: %w", rErr)
		}
		_, aErr := ApplyBillingStageTx(tx, sLedger.ID, func(t *gorm.DB, _ *TaskBillingLedger) error {
			return settleOrRefundFunds(t, snap, stage, amount, exec, cmd.Now)
		})
		if aErr != nil {
			return aErr
		}
	}

	// Step 5: Legacy Task projection (status, progress, finish_time).
	if err := tx.Model(&Task{}).Where("id = ?", exec.TaskDBID).
		Updates(map[string]any{
			"status":      taskStatusFromExecutionState(cmd.ToState),
			"progress":    "100%",
			"finish_time": cmd.Now,
		}).Error; err != nil {
		return fmt.Errorf("finalize: update task projection: %w", err)
	}

	return nil
}

// taskStatusFromExecutionState maps the §9.1 execution state to the legacy
// Task.Status value (§7.3: "Task.Status 只使用现有兼容值").
func taskStatusFromExecutionState(state ImageTaskExecutionState) string {
	switch state {
	case ImageTaskStateCompleted:
		return string(TaskStatusSuccess)
	case ImageTaskStateFailed, ImageTaskStateCancelled:
		return string(TaskStatusFailure)
	default:
		return string(TaskStatusNotStart) // should not reach here (terminal-only)
	}
}

// settleOrRefundFunds performs the real wallet/subscription/token reversal
// (P1-1). It reads the funding_source from the validated V1 snapshot and
// applies the delta to the same funding rows the reserve touched. The token
// always gets the delta (mirroring the reserve pattern), plus the funding
// source gets its own delta. free funding source skips fund changes.
func settleOrRefundFunds(tx *gorm.DB, snap ImageTaskBillingSnapshotV1, stage TaskBillingStage, amount int, exec ImageTaskExecution, now int64) error {
	if amount <= 0 {
		return nil
	}
	// Token delta (limited token only; unlimited frozen, no change).
	if snap.CreationTokenID != 0 && snap.TokenCacheDigest != "" {
		var token Token
		if err := tx.Where("id = ? AND user_id = ?", snap.CreationTokenID, snap.OwnerUserID).First(&token).Error; err != nil {
			return fmt.Errorf("%w: settle token lookup: %v", ErrImageTaskBillingData, err)
		}
		if !token.UnlimitedQuota {
			if stage == TaskBillingSettle {
				// Settle: deduct additional quota.
				res := tx.Model(&Token{}).Where("id = ? AND remain_quota >= ?", snap.CreationTokenID, amount).
					Updates(map[string]any{
						"remain_quota":  gorm.Expr("remain_quota - ?", amount),
						"used_quota":    gorm.Expr("used_quota + ?", amount),
						"accessed_time": now,
					})
				if res.Error != nil {
					return fmt.Errorf("%w: settle token: %v", ErrImageTaskBillingData, res.Error)
				}
				if res.RowsAffected != 1 {
					return ErrImageTaskTokenInvalid
				}
			} else {
				// Refund: return quota.
				res := tx.Model(&Token{}).Where("id = ?", snap.CreationTokenID).
					Updates(map[string]any{
						"remain_quota":  gorm.Expr("remain_quota + ?", amount),
						"used_quota":    gorm.Expr("used_quota - ?", amount),
						"accessed_time": now,
					})
				if res.Error != nil {
					return fmt.Errorf("%w: refund token: %v", ErrImageTaskBillingData, res.Error)
				}
			}
		}
	}
	// Funding source delta.
	switch snap.FundingSource {
	case FundingSourceFree:
		return nil // free: no fund change on reserve, none on settle/refund
	case FundingSourceWallet:
		if stage == TaskBillingSettle {
			res := tx.Model(&User{}).Where("id = ? AND quota >= ?", snap.OwnerUserID, amount).
				Update("quota", gorm.Expr("quota - ?", amount))
			if res.Error != nil {
				return fmt.Errorf("%w: settle wallet: %v", ErrImageTaskBillingData, res.Error)
			}
			if res.RowsAffected != 1 {
				return ErrImageTaskWalletInsufficient
			}
		} else {
			res := tx.Model(&User{}).Where("id = ?", snap.OwnerUserID).
				Update("quota", gorm.Expr("quota + ?", amount))
			if res.Error != nil {
				return fmt.Errorf("%w: refund wallet: %v", ErrImageTaskBillingData, res.Error)
			}
		}
	case FundingSourceSubscription:
		if snap.SubscriptionID <= 0 {
			return fmt.Errorf("settle: snapshot has subscription source but subscription_id=0")
		}
		var sub UserSubscription
		if err := tx.First(&sub, snap.SubscriptionID).Error; err != nil {
			return fmt.Errorf("%w: settle sub lookup: %v", ErrImageTaskBillingData, err)
		}
		if stage == TaskBillingRefund {
			sub.AmountUsed -= int64(amount)
			if sub.AmountUsed < 0 {
				sub.AmountUsed = 0
			}
		}
		// Settle for subscription: additional charge beyond reserve.
		// amount_used was already incremented during reserve; for settle we
		// add the delta if it fits.
		if stage == TaskBillingSettle && sub.AmountTotal > 0 {
			if sub.AmountUsed+int64(amount) > sub.AmountTotal {
				return ErrImageTaskSubscriptionInsufficient
			}
			sub.AmountUsed += int64(amount)
		}
		if err := tx.Save(&sub).Error; err != nil {
			return fmt.Errorf("%w: settle sub save: %v", ErrImageTaskBillingData, err)
		}
	}
	return nil
}
