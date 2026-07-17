package model

import (
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
	ErrImageTaskIdempotencyConflict  = errors.New("idempotency key reused with a different request")
	ErrImageTaskInFlightCap          = errors.New("per-user in-flight image task cap reached")
	ErrImageTaskInsufficientQuota    = errors.New("insufficient quota to reserve image task")
	ErrImageTaskInsufficientToken    = errors.New("insufficient token quota to reserve image task")
	ErrImageTaskNoActiveSubscription = errors.New("no active subscription to reserve image task")
	ErrImageTaskInsufficientSub      = errors.New("subscription quota insufficient to reserve image task")
)

// ImageTaskBillingSnapshot is the §7.4 typed, frozen-at-reserve billing fact
// written to the task billing ledger. It replaces the placeholder json blobs
// (map{"u":5}) with a strongly-validated struct so settle/refund can reconstruct
// the exact creation-time charge from the durable ledger. Core fields are
// required and validated; the optional price-ratio/attribution/clamp fields come
// from price resolution (caller) and default to zero until that layer is wired.
// FundingSource and SubscriptionID are filled AFTER the funding choice, before
// the ledger stage is marked applied, so the durable record matches the actual
// deduction.
type ImageTaskBillingSnapshot struct {
	OwnerUserID       int    `json:"owner_user_id"`
	Group             string `json:"group"`
	TokenID           int    `json:"token_id"`
	Operation         string `json:"operation"`
	FundingSource     string `json:"funding_source"` // wallet|subscription; "" until chosen in tx
	SubscriptionID    int    `json:"subscription_id"`
	ReserveQuota      int    `json:"reserve_quota"`
	ChannelRevisionID int64  `json:"channel_revision_id"`
	// Optional price-resolution fields (zero/"" = not yet wired).
	ModelRatioVersion int    `json:"model_ratio_version,omitempty"`
	Attribution       string `json:"attribution,omitempty"`
	QuotaClamp        int    `json:"quota_clamp,omitempty"`
}

// Validate checks the pre-choice core fields. FundingSource is validated after
// the funding choice (it is empty at intent time and set to wallet/subscription
// inside the create transaction).
func (s ImageTaskBillingSnapshot) Validate() error {
	if s.OwnerUserID <= 0 {
		return errors.New("billing snapshot: owner_user_id required")
	}
	if s.Operation == "" {
		return errors.New("billing snapshot: operation required")
	}
	if s.ReserveQuota < 0 {
		return errors.New("billing snapshot: reserve_quota must not be negative")
	}
	return nil
}

// ImageTaskCreateIntent captures everything CreateImageTaskAtomic needs for one
// §6.1 create. The caller resolves price into ReserveQuota, reads the user's
// billing preference into BillingPreference, and builds the §7.4 typed
// BillingSnapshot; this function owns the transactional create + cap enforcement
// + the full funding-source/token reserve aggregate (wallet | subscription,
// grounded in BillingSession), all in one transaction.
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
	// TokenID is the creation token whose limited quota is also reserved (0 =
	// no token). Unlimited tokens are frozen but not deducted.
	TokenID           int
	BillingPreference string // "", wallet_only, subscription_only, wallet_first, subscription_first
	BillingSnapshot   ImageTaskBillingSnapshot
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
	var reserveWalletUsed bool
	var reserveTokenKey string
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
		// image task so the legacy poller excludes it (§7.3). PrivateData freezes
		// the funding decision so settle/refund (P3-E/F) can reconstruct it.
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
			PrivateData: TaskPrivateData{
				BillingSource: ImageTaskBillingSourceWallet,
				TokenId:       intent.TokenID,
			},
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
		wu, tk, rerr := reserveImageTaskInTx(tx, task, intent)
		if rerr != nil {
			return rerr
		}
		reserveWalletUsed = wu
		reserveTokenKey = tk

		outcome.Task = task
		outcome.Execution = exec
		outcome.Created = true
		return nil
	})
	if err != nil {
		return ImageTaskCreateOutcome{}, err
	}
	// Post-commit cache convergence (P1-4): decrement the Redis quota cache for
	// the actual funding sources deducted in the committed reserve, using the
	// cache effect captured inside the transaction (no post-commit DB re-read).
	// Only for Created=true; replay/rollback do not touch cache. Failures are
	// logged (not silently swallowed) so stale-cache drift is observable.
	if outcome.Created {
		convergeImageTaskReserveCache(intent, reserveWalletUsed, reserveTokenKey)
	}
	return outcome, nil
}

// convergeImageTaskReserveCache decrements the Redis quota cache for the actual
// wallet/token sources deducted in the committed reserve. It uses the cache
// effect (walletUsed + tokenKey) captured inside the transaction — no
// post-commit DB re-read. Errors are logged via common.SysError (not silently
// swallowed) so stale-cache drift is observable and debuggable (P1-4).
func convergeImageTaskReserveCache(intent ImageTaskCreateIntent, walletUsed bool, tokenKey string) {
	if !common.RedisEnabled {
		return
	}
	if walletUsed {
		if err := cacheDecrUserQuota(intent.OwnerUserID, int64(intent.ReserveQuota)); err != nil {
			common.SysError(fmt.Sprintf("image task reserve cache: user quota convergence failed (user=%d, amount=%d): %v", intent.OwnerUserID, intent.ReserveQuota, err))
		}
	}
	if tokenKey != "" {
		if err := cacheDecrTokenQuota(tokenKey, int64(intent.ReserveQuota)); err != nil {
			common.SysError(fmt.Sprintf("image task reserve cache: token quota convergence failed (amount=%d): %v", intent.ReserveQuota, err))
		}
	}
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
	// CreationTokenID and TokenID must refer to the same token when both set,
	// so a caller cannot freeze one token on the execution row while deducting
	// another (P1-2 cross-owner/cross-token gap).
	if intent.TokenID != 0 && intent.CreationTokenID != 0 && intent.TokenID != intent.CreationTokenID {
		return fmt.Errorf("image task create: token id mismatch (token=%d, creation_token=%d)", intent.TokenID, intent.CreationTokenID)
	}
	if err := intent.BillingSnapshot.Validate(); err != nil {
		return err
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
// than hand-rolled. The apply callback performs the tx-aware fund + token
// deduction in the create transaction, mirroring BillingSession.preConsume's
// order (token first, then funding): if the wallet deduction fails the whole
// transaction — including the token deduction — rolls back, so no manual
// IncreaseTokenQuota rollback is needed (§7.4 same-tx invariant).
//
// Funding source: today the wallet path (BillingSession default for a user
// without subscription). The subscription funding source (P1-1 remaining
// slice) replaces deductWalletInTx with the tx-aware subscription analogue;
// the frozen projection's BillingSource is updated then.
// Funding-source preference values, mirroring the user billing preference
// (NewBillingSession). The caller reads them from UserSetting and passes them
// in; the empty default is subscription_first.
const (
	ImageTaskBillingPrefWalletOnly        = "wallet_only"
	ImageTaskBillingPrefSubscriptionOnly  = "subscription_only"
	ImageTaskBillingPrefWalletFirst       = "wallet_first"
	ImageTaskBillingPrefSubscriptionFirst = "subscription_first"
)

func reserveImageTaskInTx(tx *gorm.DB, task *Task, intent ImageTaskCreateIntent) (walletUsed bool, tokenKey string, err error) {
	// Build the snapshot from the authoritative intent (P1-3): core fields
	// (owner/group/token/operation/reserve/channel_revision) are overwritten from
	// intent regardless of what the caller put in BillingSnapshot; only
	// price-resolution extras (model_ratio_version/attribution/clamp) are
	// preserved from the caller.
	snapshot := intent.BillingSnapshot
	snapshot.OwnerUserID = intent.OwnerUserID
	snapshot.Group = intent.Group
	snapshot.TokenID = intent.TokenID
	snapshot.Operation = intent.Operation
	snapshot.ReserveQuota = intent.ReserveQuota
	snapshot.ChannelRevisionID = intent.ChannelRevisionID
	snapshotJSON, mErr := common.Marshal(snapshot)
	if mErr != nil {
		return false, "", fmt.Errorf("reserve ledger snapshot marshal: %w", mErr)
	}
	_, ledger, err := RecordBillingStage(tx, TaskBillingStageIntent{
		TaskDBID:    task.ID,
		Stage:       TaskBillingReserve,
		Snapshot:    snapshotJSON,
		QuotaAmount: intent.ReserveQuota,
	})
	if err != nil {
		return false, "", fmt.Errorf("reserve ledger record: %w", err)
	}
	var fundingSource string
	var subscriptionID int
	if _, err := ApplyBillingStageTx(tx, ledger.ID, func(t *gorm.DB, _ *TaskBillingLedger) error {
		// Token first (BillingSession.preConsume order); a funding failure rolls
		// the token deduction back with the transaction — no manual rollback.
		key, terr := deductTokenQuotaInTx(t, intent.OwnerUserID, intent.TokenID, intent.ReserveQuota, intent.Now)
		if terr != nil {
			return terr
		}
		tokenKey = key // non-empty only for limited+deducted (cache convergence)
		source, sid, ferr := deductFundingInTx(t, intent)
		if ferr != nil {
			return ferr
		}
		fundingSource = source
		subscriptionID = sid
		walletUsed = source == ImageTaskBillingSourceWallet
		// Write the FINAL snapshot (with the actual funding source + subscription
		// id) to the ledger, in this transaction, before the CAS marks it applied.
		final := snapshot
		final.FundingSource = source
		final.SubscriptionID = sid
		finalJSON, fmErr := common.Marshal(final)
		if fmErr != nil {
			return fmErr
		}
		if uErr := t.Model(&TaskBillingLedger{}).Where("id = ?", ledger.ID).Update("billing_snapshot", finalJSON).Error; uErr != nil {
			return fmt.Errorf("reserve ledger snapshot finalize: %w", uErr)
		}
		return nil
	}); err != nil {
		return false, "", err
	}
	// Freeze the actual funding source on the task projection.
	if fundingSource == ImageTaskBillingSourceSubscription {
		task.PrivateData.BillingSource = ImageTaskBillingSourceSubscription
		task.PrivateData.SubscriptionId = subscriptionID
		if err := tx.Model(&Task{}).Where("id = ?", task.ID).Update("private_data", task.PrivateData).Error; err != nil {
			return false, "", fmt.Errorf("reserve projection update: %w", err)
		}
	}
	return walletUsed, tokenKey, nil
}

// deductFundingInTx chooses the funding source per the user's billing preference
// (mirroring NewBillingSession) and deducts it inside the transaction. It
// returns the source used and the subscription id (0 for wallet). A 0 reserve
// deducts nothing.
//
// subscription_first (default) respects the subscription's frozen
// allow_wallet_overflow flag (from the plan at purchase): a strict subscription
// (allow_wallet_overflow=false) that is insufficient returns subscription
// insufficient WITHOUT falling back to wallet; allow_wallet_overflow=true falls
// back to wallet; no active subscription falls back to wallet.
func deductFundingInTx(tx *gorm.DB, intent ImageTaskCreateIntent) (string, int, error) {
	if intent.ReserveQuota <= 0 {
		return ImageTaskBillingSourceWallet, 0, nil
	}
	tryWallet := func() error { return deductWalletInTx(tx, intent.OwnerUserID, intent.ReserveQuota) }
	trySub := func() (int, bool, error) {
		return deductSubscriptionInTx(tx, intent.OwnerUserID, int64(intent.ReserveQuota), intent.Now)
	}
	switch intent.BillingPreference {
	case ImageTaskBillingPrefWalletOnly:
		return ImageTaskBillingSourceWallet, 0, tryWallet()
	case ImageTaskBillingPrefSubscriptionOnly:
		sid, _, err := trySub()
		if err != nil {
			return "", 0, err
		}
		return ImageTaskBillingSourceSubscription, sid, nil
	case ImageTaskBillingPrefWalletFirst:
		if err := tryWallet(); err == nil {
			return ImageTaskBillingSourceWallet, 0, nil
		} else if !errors.Is(err, ErrImageTaskInsufficientQuota) {
			return "", 0, err
		}
		sid, _, err := trySub()
		if err == nil {
			return ImageTaskBillingSourceSubscription, sid, nil
		}
		return "", 0, err
	default: // subscription_first (and "")
		sid, overflow, err := trySub()
		if err == nil {
			return ImageTaskBillingSourceSubscription, sid, nil
		}
		// strict (allow_wallet_overflow=false) + insufficient → do NOT touch wallet
		if errors.Is(err, ErrImageTaskInsufficientSub) && !overflow {
			return "", 0, err
		}
		// no-active, allow-overflow, or unexpected → wallet fallback
		if !errors.Is(err, ErrImageTaskNoActiveSubscription) && !errors.Is(err, ErrImageTaskInsufficientSub) {
			return "", 0, err
		}
		if e := tryWallet(); e == nil {
			return ImageTaskBillingSourceWallet, 0, nil
		} else {
			return "", 0, e
		}
	}
}

// deductSubscriptionInTx is the tx-aware analogue of
// PreConsumeUserSubscription's core: lock the owner's active subscriptions,
// apply any period reset, and decrement the first subscription with enough
// remaining quota. It mirrors the lockForUpdate + plan reset + amount_used
// deduction; image-task idempotency is the execution key (a replay never enters
// the transaction), so the separate requestId pre-consume record is not needed
// here — settle/refund (P3-E/F) reconstruct via the frozen SubscriptionId.
func deductSubscriptionInTx(tx *gorm.DB, ownerUserID int, amount int64, now int64) (subID int, allowOverflow bool, err error) {
	var subs []UserSubscription
	if err := lockForUpdate(tx).
		Where("user_id = ? AND status = ? AND end_time > ?", ownerUserID, "active", now).
		Order("end_time asc, id asc").
		Find(&subs).Error; err != nil {
		return 0, false, fmt.Errorf("reserve subscription lookup: %w", err)
	}
	if len(subs) == 0 {
		return 0, false, ErrImageTaskNoActiveSubscription
	}
	for _, candidate := range subs {
		sub := candidate
		plan, err := getSubscriptionPlanByIdTx(tx, sub.PlanId)
		if err != nil {
			return 0, false, fmt.Errorf("reserve subscription plan lookup: %w", err)
		}
		if err := maybeResetUserSubscriptionWithPlanTx(tx, &sub, plan, now); err != nil {
			return 0, false, fmt.Errorf("reserve subscription period reset: %w", err)
		}
		if sub.AmountTotal > 0 && sub.AmountTotal-sub.AmountUsed < amount {
			continue
		}
		sub.AmountUsed += amount
		if err := tx.Save(&sub).Error; err != nil {
			return 0, false, fmt.Errorf("reserve subscription deduction: %w", err)
		}
		return sub.Id, false, nil
	}
	// All active subs insufficient: aggregate overflow — wallet fallback is
	// allowed only when ALL active subscriptions allow it (mirror
	// UserActiveSubscriptionsAllowWalletOverflow: ANY strict → no fallback).
	allAllow := true
	for _, s := range subs {
		if !s.AllowWalletOverflow {
			allAllow = false
			break
		}
	}
	return 0, allAllow, ErrImageTaskInsufficientSub
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

// deductTokenQuotaInTx reads, validates, and optionally decrements the token in
// one transaction-safe step. ALL tokens — including unlimited — are read with
// full ownership/status/expiry constraints (id + user_id=owner + status=enabled
// + not expired) BEFORE the unlimited branch, so a disabled, expired, or
// cross-owner unlimited token cannot be frozen onto a new task (P1-1). It
// returns the token Key (for post-commit cache convergence) when the token is
// accessed; "" when no token (TokenID=0).
func deductTokenQuotaInTx(tx *gorm.DB, ownerUserID, tokenID, amount int, now int64) (string, error) {
	if tokenID == 0 || amount <= 0 {
		return "", nil
	}
	// Full-constraint read: covers owner, enabled, and expiry for ALL tokens
	// (including unlimited) before branching. A wrong-owner / disabled /
	// expired token is rejected here, not silently frozen.
	var token Token
	err := tx.Where("id = ? AND user_id = ? AND status = ? AND (expired_time = ? OR expired_time > ?)",
		tokenID, ownerUserID, common.TokenStatusEnabled, -1, now).
		First(&token).Error
	if err != nil {
		return "", fmt.Errorf("reserve token lookup (owner/status/expiry): %w", err)
	}
	if token.UnlimitedQuota {
		return "", nil // frozen, not deducted; no cache change needed
	}
	deducted := tx.Model(&Token{}).
		Where("id = ? AND remain_quota >= ?", tokenID, amount).
		Updates(map[string]any{
			"remain_quota":  gorm.Expr("remain_quota - ?", amount),
			"used_quota":    gorm.Expr("used_quota + ?", amount),
			"accessed_time": now,
		})
	if deducted.Error != nil {
		return "", fmt.Errorf("reserve token deduction: %w", deducted.Error)
	}
	if deducted.RowsAffected != 1 {
		return "", ErrImageTaskInsufficientToken
	}
	return token.Key, nil
}

// ImageTaskBillingSource* are the funding-source values frozen on
// Task.PrivateData.BillingSource at reserve time, mirroring
// service.BillingSourceWallet/Subscription. Defined in model so the projection
// does not import service.
const (
	ImageTaskBillingSourceWallet       = "wallet"
	ImageTaskBillingSourceSubscription = "subscription"
)

// GenerateImageTaskPublicID returns a new imgtask_ public id.
func GenerateImageTaskPublicID() string {
	key, _ := common.GenerateRandomCharsKey(24)
	return "imgtask_" + key
}
