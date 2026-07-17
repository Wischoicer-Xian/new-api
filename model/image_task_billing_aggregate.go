package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"gorm.io/gorm"
)

// Image task aggregate error sentinels (§5.5/§5.6).
var (
	ErrImageTaskIdempotencyConflict      = errors.New("idempotency key reused with a different request")
	ErrImageTaskInFlightCapReached       = errors.New("per-user in-flight image task cap reached")
	ErrImageTaskWalletInsufficient       = errors.New("wallet quota insufficient for image task reserve")
	ErrImageTaskSubscriptionInsufficient = errors.New("subscription quota insufficient for image task reserve")
	ErrImageTaskNoActiveSubscription     = errors.New("no active subscription for image task reserve")
	ErrImageTaskTokenInvalid             = errors.New("creation token is invalid, disabled, expired, or not owned by the user")
	ErrImageTaskBillingData              = errors.New("billing data query or update error")
	ErrImageTaskBillingRetryExhausted    = errors.New("image task billing aggregate retry exhausted")
)

// ImageTaskReserveCommand is the immutable command (§5.5). It contains ONLY
// facts the caller resolved that cannot be read from the DB. Billing preference,
// funding source, subscription ID, and snapshot JSON are NOT included; the
// aggregate resolves them from locked rows.
type ImageTaskReserveCommand struct {
	OwnerUserID       int
	Operation         string
	IdempotencyKey    string
	RequestHash       string
	ChannelRevisionID int64
	ExecutionMode     string
	AdapterVersion    string
	CreationTokenID   int
	Price             *ImageTaskPriceResolution
	Attribution       json.RawMessage
	RequestID         string
	UpstreamRequestID string
	Now               int64
}

// ImageTaskReserveOutcome is the result of a reserve command.
type ImageTaskReserveOutcome struct {
	Task                *Task
	Execution           *ImageTaskExecution
	Replayed            bool
	FundingSource       string
	AppliedReserveQuota int
	SubscriptionID      int
	// CacheEffect contains the keys to delete from Redis after commit (§5.8).
	CacheEffect ImageTaskCacheEffect
}

// ImageTaskCacheEffect captures which Redis keys need invalidation after commit.
type ImageTaskCacheEffect struct {
	DeleteUserQuota bool   // delete user:<id> if wallet was deducted
	OwnerUserID     int    // user ID for user quota cache key
	DeleteTokenKey  string // HMAC digest of token key for token cache deletion
}

// ReserveImageTask is the single aggregate entry point (§5.5). It executes the
// 11-step create: validate → lock owner → idempotency → cap → token → funding →
// snapshot → Task/execution/ledger → ApplyBillingStageTx → cache effect.
//
// The command's Now/Price/Hash are frozen before the first attempt (no
// re-reads during retry). Business errors, unique losers, and Redis errors are
// NOT retried. The caller decides retry policy for transient DB errors.
func ReserveImageTask(ctx context.Context, cmd ImageTaskReserveCommand) (ImageTaskReserveOutcome, error) {
	if err := validateReserveCommand(cmd); err != nil {
		return ImageTaskReserveOutcome{}, err
	}

	// Step 0: fail-closed if Redis enabled but TTL invalid (§5.8.1).
	if err := CheckCacheSafety(); err != nil {
		return ImageTaskReserveOutcome{}, err
	}

	// Freeze now from command (do not call time.Now() inside the tx).
	now := cmd.Now
	if now == 0 {
		now = time.Now().Unix()
	}

	outcome := ImageTaskReserveOutcome{}
	var cacheEffect ImageTaskCacheEffect

	const maxAttempts = 3
	backoffs := []int64{0, 50, 100} // ms; first attempt no delay
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 && backoffs[attempt] > 0 {
			time.Sleep(time.Duration(backoffs[attempt]) * time.Millisecond)
		}
		lastErr = DB.Transaction(func(tx *gorm.DB) error {
			// Step 2: Lock owner user (first DB statement; MySQL/PG FOR UPDATE,
			// SQLite serial writer). This is the snapshot-establishing fence.
			var fence User
			if err := lockForUpdate(tx).Select("id").First(&fence, cmd.OwnerUserID).Error; err != nil {
				return fmt.Errorf("reserve image task: lock owner fence: %w", err)
			}

			// Step 3: Idempotency current read under fence.
			stored := &ImageTaskExecution{}
			lookup := tx.Where("owner_user_id = ? AND operation = ? AND idempotency_key = ?",
				cmd.OwnerUserID, cmd.Operation, cmd.IdempotencyKey).First(stored)
			if lookup.Error == nil {
				return resolveReplayOrConflict(tx, stored, cmd, &outcome)
			}
			if !errors.Is(lookup.Error, gorm.ErrRecordNotFound) {
				return fmt.Errorf("%w: idempotency lookup: %v", ErrImageTaskBillingData, lookup.Error)
			}

			// Step 4: In-flight cap (§6.1).
			count, err := CountInFlightImageTasksByOwner(tx, cmd.OwnerUserID)
			if err != nil {
				return fmt.Errorf("%w: count in-flight: %v", ErrImageTaskBillingData, err)
			}
			if count >= int64(constant.MaxImageTasksPerUser) {
				return ErrImageTaskInFlightCapReached
			}

			// Step 5: Token lock + validate (reserve=0 also executes).
			tokenKey, tokenDigest, err := lockAndValidateToken(tx, cmd, now)
			if err != nil {
				return err
			}

			// Steps 6-7: Funding plan (no balance changes) + snapshot construction.
			fundingSource, subscriptionID, appliedReserve, selectedSub, ferr :=
				planFunding(tx, cmd, fence, now)
			if ferr != nil {
				return ferr
			}

			// Build snapshot from price + resolved funding (§5.3).
			attribution, err := parseAttribution(cmd.Attribution)
			if err != nil {
				return err
			}
			snapshot, err := buildSnapshotFromPriceAndFunding(
				cmd.Price, cmd.OwnerUserID, cmd.CreationTokenID, cmd.Operation,
				cmd.ChannelRevisionID, tokenDigest, attribution,
				cmd.RequestID, cmd.UpstreamRequestID,
				fundingSource, subscriptionID, appliedReserve,
			)
			if err != nil {
				return err
			}
			snapshotJSON, err := snapshot.Marshal()
			if err != nil {
				return fmt.Errorf("reserve image task: snapshot marshal: %w", err)
			}

			// Step 8: Create Task + execution + reserve ledger.
			task := &Task{
				TaskID:     GenerateTaskID(),
				UserId:     cmd.OwnerUserID,
				Group:      cmd.Price.ResolvedGroup(),
				Platform:   constant.TaskPlatformWischoicerImage,
				Action:     cmd.Operation,
				Status:     TaskStatusNotStart,
				Progress:   "0%",
				SubmitTime: now,
				Quota:      cmd.Price.FormulaReserveQuota(),
			}
			if err := tx.Create(task).Error; err != nil {
				return fmt.Errorf("%w: create task: %v", ErrImageTaskBillingData, err)
			}

			exec := &ImageTaskExecution{
				PublicTaskID:      GenerateImageTaskPublicID(),
				TaskDBID:          task.ID,
				OwnerUserID:       cmd.OwnerUserID,
				CreationTokenID:   cmd.CreationTokenID,
				Operation:         cmd.Operation,
				IdempotencyKey:    cmd.IdempotencyKey,
				RequestHash:       cmd.RequestHash,
				State:             ImageTaskStateQueued,
				ChannelRevisionID: cmd.ChannelRevisionID,
				ExecutionMode:     cmd.ExecutionMode,
				AdapterVersion:    cmd.AdapterVersion,
				CreatedAt:         now,
				UpdatedAt:         now,
			}
			if err := tx.Create(exec).Error; err != nil {
				// Duplicate-key: unique loser. Return billing-data error; caller
				// re-reads the winner outside the tx for replay/conflict.
				return fmt.Errorf("%w: duplicate idempotency key: %v", ErrImageTaskBillingData, err)
			}

			// Step 9: Reserve ledger via ApplyBillingStageTx (§5.4 single CAS).
			_, ledger, rErr := RecordBillingStage(tx, TaskBillingStageIntent{
				TaskDBID:    task.ID,
				Stage:       TaskBillingReserve,
				Snapshot:    snapshotJSON,
				QuotaAmount: appliedReserve,
			})
			if rErr != nil {
				return fmt.Errorf("%w: record reserve stage: %v", ErrImageTaskBillingData, rErr)
			}

			_, aErr := ApplyBillingStageTx(tx, ledger.ID, func(t *gorm.DB, _ *TaskBillingLedger) error {
				// §5.5 step 9 sub-steps:
				if fundingSource == FundingSourceFree {
					return nil // free: no fund/token deduction, amount=0 ledger
				}
				// Token deduction (limited only; unlimited frozen in step 5).
				if cmd.CreationTokenID != 0 && tokenKey != "" {
					if err := deductLimitedTokenInTx(t, cmd.CreationTokenID, cmd.OwnerUserID, appliedReserve, now); err != nil {
						return err
					}
				}
				// Funding deduction.
				switch fundingSource {
				case FundingSourceWallet:
					return deductWalletInAggregateTx(t, cmd.OwnerUserID, appliedReserve)
				case FundingSourceSubscription:
					return deductSubscriptionInAggregateTx(t, selectedSub, appliedReserve)
				}
				return nil
			})
			if aErr != nil {
				return aErr
			}

			// Set Task.PrivateData compatibility projection (§5.3: derived, not truth).
			task.PrivateData = TaskPrivateData{
				BillingSource:  fundingSource,
				TokenId:        cmd.CreationTokenID,
				SubscriptionId: subscriptionID,
			}
			if err := tx.Model(task).Where("id = ?", task.ID).Update("private_data", task.PrivateData).Error; err != nil {
				return fmt.Errorf("%w: update task projection: %v", ErrImageTaskBillingData, err)
			}

			outcome.Task = task
			outcome.Execution = exec
			outcome.FundingSource = fundingSource
			outcome.AppliedReserveQuota = appliedReserve
			outcome.SubscriptionID = subscriptionID

			// Step 11 (cache effect, captured for post-commit).
			if fundingSource == FundingSourceWallet && appliedReserve > 0 {
				cacheEffect.DeleteUserQuota = true
				cacheEffect.OwnerUserID = cmd.OwnerUserID
			}
			// Token HMAC digest for cache delete (lockAndValidateToken returns digest).
			cacheEffect.DeleteTokenKey = tokenDigest

			return nil
		})
		if lastErr == nil {
			break // success
		}
		// Duplicate-key: unique loser — aggregate re-reads winner for replay/conflict.
		if errors.Is(lastErr, ErrImageTaskBillingData) {
			stored := &ImageTaskExecution{}
			if e := DB.Where("owner_user_id = ? AND operation = ? AND idempotency_key = ?",
				cmd.OwnerUserID, cmd.Operation, cmd.IdempotencyKey).First(stored).Error; e == nil {
				outcome.Task = &Task{}
				if e := DB.First(outcome.Task, stored.TaskDBID).Error; e == nil {
					outcome.Execution = stored
					outcome.Replayed = true
					lastErr = nil
					break
				}
			}
			// Winner not found despite duplicate-key error: continue retry.
		}
		// Business errors (typed sentinels) do NOT retry.
		if isBusinessError(lastErr) {
			break
		}
	}
	if lastErr != nil {
		if errors.Is(lastErr, ErrImageTaskBillingData) {
			return ImageTaskReserveOutcome{}, ErrImageTaskBillingRetryExhausted
		}
		return ImageTaskReserveOutcome{}, lastErr
	}

	// Post-commit cache delete (§5.8): only for Created (non-replay).
	if outcome.Task != nil && !outcome.Replayed {
		ApplyCacheEffect(cacheEffect, nil)
	}
	outcome.CacheEffect = cacheEffect
	return outcome, nil
}

// GenerateImageTaskPublicID returns a new imgtask_ public id.
func GenerateImageTaskPublicID() string {
	key, _ := common.GenerateRandomCharsKey(24)
	return "imgtask_" + key
}

// isBusinessError reports whether err is one of the typed billing sentinels
// that must NOT be retried (§5.5 step 10: "业务错误不重试").
func isBusinessError(err error) bool {
	switch {
	case errors.Is(err, ErrImageTaskIdempotencyConflict),
		errors.Is(err, ErrImageTaskInFlightCapReached),
		errors.Is(err, ErrImageTaskWalletInsufficient),
		errors.Is(err, ErrImageTaskSubscriptionInsufficient),
		errors.Is(err, ErrImageTaskNoActiveSubscription),
		errors.Is(err, ErrImageTaskTokenInvalid),
		errors.Is(err, ErrUnsupportedImageTaskPricingFacts),
		errors.Is(err, ErrImageTaskCacheSafetyMisconfigured):
		return true
	}
	return false
}

func validateReserveCommand(cmd ImageTaskReserveCommand) error {
	if cmd.OwnerUserID <= 0 {
		return errors.New("reserve image task: owner_user_id required")
	}
	if cmd.IdempotencyKey == "" {
		return errors.New("reserve image task: idempotency_key required")
	}
	if cmd.RequestHash == "" {
		return errors.New("reserve image task: request_hash required")
	}
	if cmd.Operation != ImageTaskOperationGeneration && cmd.Operation != ImageTaskOperationEdit {
		return fmt.Errorf("reserve image task: invalid operation %q", cmd.Operation)
	}
	if cmd.Price == nil {
		return errors.New("reserve image task: price resolution required")
	}
	return nil
}

func resolveReplayOrConflict(tx *gorm.DB, stored *ImageTaskExecution, cmd ImageTaskReserveCommand, outcome *ImageTaskReserveOutcome) error {
	if stored.RequestHash != cmd.RequestHash {
		return ErrImageTaskIdempotencyConflict
	}
	task := &Task{}
	if err := tx.First(task, stored.TaskDBID).Error; err != nil {
		return fmt.Errorf("%w: load replayed task: %v", ErrImageTaskBillingData, err)
	}
	outcome.Task = task
	outcome.Execution = stored
	outcome.Replayed = true
	return nil
}

// lockAndValidateToken reads the token under the owner fence with full
// constraints (id + owner + enabled + not expired). Returns the token key
// (for cache effect) and its HMAC digest (for snapshot). Returns empty strings
// when CreationTokenID == 0 (no token).
func lockAndValidateToken(tx *gorm.DB, cmd ImageTaskReserveCommand, now int64) (key, digest string, err error) {
	if cmd.CreationTokenID == 0 {
		return "", "", nil
	}
	var token Token
	if err := lockForUpdate(tx).
		Where("id = ? AND user_id = ? AND status = ? AND (expired_time = ? OR expired_time > ?)",
			cmd.CreationTokenID, cmd.OwnerUserID, common.TokenStatusEnabled, -1, now).
		First(&token).Error; err != nil {
		return "", "", ErrImageTaskTokenInvalid
	}
	digest = common.GenerateHMAC(token.Key)
	return token.Key, digest, nil
}

// deductLimitedTokenInTx decrements remain_quota, bumps used_quota and
// accessed_time for a limited token. Called inside ApplyBillingStageTx callback.
// Returns nil for unlimited (no deduction, owner/status/expiry already validated).
func deductLimitedTokenInTx(tx *gorm.DB, tokenID, ownerUserID, amount int, now int64) error {
	if amount <= 0 {
		return nil
	}
	var token Token
	if err := tx.Where("id = ? AND user_id = ?", tokenID, ownerUserID).First(&token).Error; err != nil {
		return fmt.Errorf("%w: token lookup: %v", ErrImageTaskBillingData, err)
	}
	if token.UnlimitedQuota {
		return nil
	}
	res := tx.Model(&Token{}).
		Where("id = ? AND remain_quota >= ?", tokenID, amount).
		Updates(map[string]any{
			"remain_quota":  gorm.Expr("remain_quota - ?", amount),
			"used_quota":    gorm.Expr("used_quota + ?", amount),
			"accessed_time": now,
		})
	if res.Error != nil {
		return fmt.Errorf("%w: token deduction: %v", ErrImageTaskBillingData, res.Error)
	}
	if res.RowsAffected != 1 {
		return ErrImageTaskTokenInvalid
	}
	return nil
}

// deductWalletInAggregateTx conditionally decrements user.quota (prevents negative).
func deductWalletInAggregateTx(tx *gorm.DB, ownerUserID, amount int) error {
	if amount <= 0 {
		return nil
	}
	res := tx.Model(&User{}).
		Where("id = ? AND quota >= ?", ownerUserID, amount).
		Update("quota", gorm.Expr("quota - ?", amount))
	if res.Error != nil {
		return fmt.Errorf("%w: wallet deduction: %v", ErrImageTaskBillingData, res.Error)
	}
	if res.RowsAffected != 1 {
		return ErrImageTaskWalletInsufficient
	}
	return nil
}
