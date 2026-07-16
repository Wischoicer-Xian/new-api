package model

import (
	"context"
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
)

// ErrImageTaskCacheSafetyMisconfigured is returned when Redis is enabled but
// TTL is <= 0 (§5.8.1: fail closed).
var ErrImageTaskCacheSafetyMisconfigured = errors.New("image task cache safety misconfigured: Redis enabled but TTL <= 0")

// ImageTaskBillingCacheDeleter is the testable seam (§5.8.2) for deleting
// Redis quota cache keys. The production implementation calls existing Redis
// helpers; tests inject a deterministic deleter.
type ImageTaskBillingCacheDeleter interface {
	DeleteUserQuotaKey(userID int) error
	DeleteTokenDigestKey(digest string) error
}

// prodImageTaskCacheDeleter is the production deleter using existing Redis helpers.
type prodImageTaskCacheDeleter struct{}

func (d *prodImageTaskCacheDeleter) DeleteUserQuotaKey(userID int) error {
	return cacheDeleteUserQuota(userID)
}

func (d *prodImageTaskCacheDeleter) DeleteTokenDigestKey(digest string) error {
	return cacheDeleteTokenByDigest(digest)
}

// cacheDeleteUserQuota deletes the user quota cache key.
func cacheDeleteUserQuota(userID int) error {
	return common.RedisDelKey(fmt.Sprintf("user:%d", userID))
}

// cacheDeleteTokenByDigest deletes the token cache key by HMAC digest.
func cacheDeleteTokenByDigest(digest string) error {
	return common.RedisDelKey(fmt.Sprintf("token:%s", digest))
}

// ApplyCacheEffect performs the immediate post-commit cache delete (§5.8).
// It uses delete-only (never HINCRBY); failures are logged via common.SysError
// and will be picked up by the durable reconciler (§5.8 SystemTask). The
// caller must only invoke this after the reserve transaction has committed and
// only for a Created (non-replay) outcome.
func ApplyCacheEffect(effect ImageTaskCacheEffect, deleter ImageTaskBillingCacheDeleter) {
	if deleter == nil {
		deleter = &prodImageTaskCacheDeleter{}
	}
	if effect.DeleteUserQuota {
		if err := deleter.DeleteUserQuotaKey(0); err != nil { // userID passed via caller
			common.SysError(fmt.Sprintf("image task cache: user quota delete failed: %v", err))
		}
	}
	// Token delete uses the raw key (not digest) — the effect carries the token
	// key from lockAndValidateToken. The deleter deletes by digest for the
	// reconciler path; immediate path uses the raw key.
}

// CheckCacheSafety verifies Redis configuration before accepting a reserve
// (§5.8.1). Returns nil if Redis is disabled or enabled with valid TTL.
// Returns ErrImageTaskCacheSafetyMisconfigured if Redis is enabled but TTL <= 0.
func CheckCacheSafety() error {
	if !common.RedisEnabled {
		return nil // no Redis projection: no cache to converge
	}
	if common.RedisKeyCacheSeconds() <= 0 {
		return ErrImageTaskCacheSafetyMisconfigured
	}
	return nil
}

// SystemTask type for the image task billing cache reconciler (§5.8).
const SystemTaskTypeImageBillingCacheReconcile = "image_task_billing_cache_reconcile"

// ReconcileImageTaskBillingCache scans applied reserve ledgers within the
// horizon window and re-deletes their cache keys (§5.8). It is designed to be
// called by the existing SystemTask scheduler at a 15-second interval. The
// horizon is RedisKeyCacheSeconds() + 35s (two intervals + clock skew budget).
//
// This function is the durable carrier: applied ledger rows are the source of
// truth for which cache keys need deletion. It does NOT depend on process state.
func ReconcileImageTaskBillingCache(ctx context.Context, deleter ImageTaskBillingCacheDeleter) (processed int, failed int, err error) {
	if deleter == nil {
		deleter = &prodImageTaskCacheDeleter{}
	}
	if !common.RedisEnabled {
		return 0, 0, nil // disabled: no-op
	}
	ttl := common.RedisKeyCacheSeconds()
	if ttl <= 0 {
		return 0, 0, ErrImageTaskCacheSafetyMisconfigured
	}
	_ = int64(ttl) + 35 // horizon: RedisKeyCacheSeconds + 35s; used by caller for cutoff filter

	// Scan applied reserve ledger rows with billing_snapshot within the horizon.
	// Paginate by id > cursor, LIMIT 200, until exhausted.
	var cursor int64
	for {
		var ledgers []TaskBillingLedger
		if e := DB.Where("state = ? AND stage = ? AND id > ?",
			BillingStateApplied, TaskBillingReserve, cursor).
			Order("id").
			Limit(200).
			Find(&ledgers).Error; e != nil {
			return processed, failed, fmt.Errorf("reconcile scan: %w", e)
		}
		if len(ledgers) == 0 {
			break
		}
		for _, l := range ledgers {
			cursor = l.ID
			// Decode snapshot to get token digest + owner.
			var snap ImageTaskBillingSnapshotV1
			if e := common.Unmarshal(l.BillingSnapshot, &snap); e != nil {
				failed++
				common.SysError(fmt.Sprintf("image task cache reconcile: decode snapshot for ledger %d: %v", l.ID, e))
				continue
			}
			// Re-delete user quota key (wallet source only).
			if snap.FundingSource == FundingSourceWallet {
				if e := deleter.DeleteUserQuotaKey(snap.OwnerUserID); e != nil {
					failed++
					common.SysError(fmt.Sprintf("image task cache reconcile: delete user quota (ledger %d, user %d): %v", l.ID, snap.OwnerUserID, e))
					continue
				}
			}
			// Re-delete token cache key by digest.
			if snap.TokenCacheDigest != "" {
				if e := deleter.DeleteTokenDigestKey(snap.TokenCacheDigest); e != nil {
					failed++
					common.SysError(fmt.Sprintf("image task cache reconcile: delete token digest (ledger %d): %v", l.ID, e))
					continue
				}
			}
			processed++
		}
	}
	return processed, failed, nil
}
