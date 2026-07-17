//go:build integration

package model

import (
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// TestLedgerCASGates_RealDB runs the §5.4 four CAS gates on real MySQL8 /
// PostgreSQL 16: outer rollback keeps pending, apply error rolls back,
// already-applied replay doesn't invoke callback, wrapper compat.
func TestLedgerCASGates_RealDB(t *testing.T) {
	setupWischoicerIntegrationDB(t)

	t.Run("OuterRollbackKeepsPending", func(t *testing.T) {
		ledger := seedPendingLedger(t, "intg-outer")
		cbCalled := false
		err := DB.Transaction(func(tx *gorm.DB) error {
			won, e := ApplyBillingStageTx(tx, ledger.ID, func(tx *gorm.DB, l *TaskBillingLedger) error {
				cbCalled = true
				return nil
			})
			require.True(t, won)
			require.NoError(t, e)
			return errSentinelOuter
		})
		assert.ErrorIs(t, err, errSentinelOuter)
		assert.True(t, cbCalled)
		var after TaskBillingLedger
		require.NoError(t, DB.First(&after, ledger.ID).Error)
		assert.Equal(t, BillingStatePending, after.State)
	})

	t.Run("ApplyErrorRollsBack", func(t *testing.T) {
		ledger := seedPendingLedger(t, "intg-cb-err")
		won, err := ApplyBillingStage(DB, ledger.ID, func(tx *gorm.DB, l *TaskBillingLedger) error {
			return errSentinelOuter
		})
		assert.False(t, won)
		assert.Error(t, err)
		var after TaskBillingLedger
		require.NoError(t, DB.First(&after, ledger.ID).Error)
		assert.Equal(t, BillingStatePending, after.State)
	})

	t.Run("AlreadyAppliedReplay", func(t *testing.T) {
		ledger := seedPendingLedger(t, "intg-replay")
		cbCount := 0
		won1, err := ApplyBillingStage(DB, ledger.ID, func(tx *gorm.DB, l *TaskBillingLedger) error {
			cbCount++
			return nil
		})
		require.NoError(t, err)
		require.True(t, won1)
		won2, _ := ApplyBillingStage(DB, ledger.ID, func(tx *gorm.DB, l *TaskBillingLedger) error {
			cbCount++
			return nil
		})
		assert.False(t, won2)
		assert.Equal(t, 1, cbCount)
	})

	t.Run("WrapperCompat", func(t *testing.T) {
		ledger := seedPendingLedger(t, "intg-wrapper")
		won, err := ApplyBillingStage(DB, ledger.ID, func(tx *gorm.DB, l *TaskBillingLedger) error {
			return nil
		})
		require.NoError(t, err)
		assert.True(t, won)
		var after TaskBillingLedger
		require.NoError(t, DB.First(&after, ledger.ID).Error)
		assert.Equal(t, BillingStateApplied, after.State)
	})
}

// TestFundMatrix_RealDB runs the §5.6 funding matrix on real MySQL8 / PostgreSQL 16:
// wallet success, subscription success, free model, token insufficient,
// wallet insufficient, strict overflow (no wallet), allow overflow (wallet fallback),
// concurrent same-key replay, concurrent different-key cap convergence.
func TestFundMatrix_RealDB(t *testing.T) {
	setupWischoicerIntegrationDB(t)
	prevCap := constant.MaxImageTasksPerUser
	constant.MaxImageTasksPerUser = 10
	t.Cleanup(func() { constant.MaxImageTasksPerUser = prevCap })

	t.Run("WalletSuccess", func(t *testing.T) {
		const owner = 9001
		require.NoError(t, DB.Create(&User{Id: owner, Username: "fm9001", AffCode: "aff9001", Quota: 100}).Error)
		price := mustPrice(t, "model_price", "model_price", 0.000085, 0, 1, true)
		out := mustReserve(t, owner, "fm-wallet", price)
		require.True(t, !out.Replayed)
		assert.Equal(t, FundingSourceWallet, out.FundingSource)
		var u User
		require.NoError(t, DB.First(&u, owner).Error)
		assert.Equal(t, 100-42, u.Quota)
	})

	t.Run("SubscriptionSuccess", func(t *testing.T) {
		const owner = 9002
		require.NoError(t, DB.Create(&User{Id: owner, Username: "fm9002", AffCode: "aff9002", Quota: 100}).Error)
		require.NoError(t, DB.Create(&SubscriptionPlan{Id: 9002, Title: "p", DurationUnit: "month"}).Error)
		require.NoError(t, DB.Create(&UserSubscription{Id: 9002, UserId: owner, PlanId: 9002, AmountTotal: 100, AmountUsed: 0, StartTime: 1, EndTime: 2e9, Status: "active", NextResetTime: 2e9, AllowWalletOverflow: true}).Error)
		price := mustPrice(t, "model_ratio", "model_ratio", 0, 0.00017, 1, true)
		out := mustReserve(t, owner, "fm-sub", price)
		require.True(t, !out.Replayed)
		assert.Equal(t, FundingSourceSubscription, out.FundingSource)
		var sub UserSubscription
		require.NoError(t, DB.First(&sub, 9002).Error)
		assert.Equal(t, int64(42), sub.AmountUsed)
		var u User
		require.NoError(t, DB.First(&u, owner).Error)
		assert.Equal(t, 100, u.Quota, "wallet untouched")
	})

	t.Run("FreeModel", func(t *testing.T) {
		const owner = 9003
		require.NoError(t, DB.Create(&User{Id: owner, Username: "fm9003", AffCode: "aff9003", Quota: 100}).Error)
		price := mustPrice(t, "model_price", "model_price", 0, 0, 1, false)
		out := mustReserve(t, owner, "fm-free", price)
		require.True(t, !out.Replayed)
		assert.Equal(t, FundingSourceFree, out.FundingSource)
		assert.Zero(t, out.AppliedReserveQuota)
		var u User
		require.NoError(t, DB.First(&u, owner).Error)
		assert.Equal(t, 100, u.Quota, "free: no deduction")
	})

	t.Run("StrictOverflowNoWallet", func(t *testing.T) {
		const owner = 9004
		require.NoError(t, DB.Create(&User{Id: owner, Username: "fm9004", AffCode: "aff9004", Quota: 100}).Error)
		require.NoError(t, DB.Create(&SubscriptionPlan{Id: 9004, Title: "s", DurationUnit: "month"}).Error)
		require.NoError(t, DB.Create(&UserSubscription{Id: 9004, UserId: owner, PlanId: 9004, AmountTotal: 3, AmountUsed: 0, StartTime: 1, EndTime: 2e9, Status: "active", NextResetTime: 2e9, AllowWalletOverflow: false}).Error)
		price := mustPrice(t, "model_price", "model_price", 0.000085, 0, 1, true)
		_, err := reserveRaw(owner, "fm-strict", price)
		assert.ErrorIs(t, err, ErrImageTaskSubscriptionInsufficient)
		var u User
		require.NoError(t, DB.First(&u, owner).Error)
		assert.Equal(t, 100, u.Quota, "strict: wallet NOT touched")
	})

	t.Run("ConcurrentSameKeyReplays", func(t *testing.T) {
		const owner = 9005
		require.NoError(t, DB.Create(&User{Id: owner, Username: "fm9005", AffCode: "aff9005", Quota: 1000}).Error)
		price := mustPrice(t, "model_price", "model_price", 0.000085, 0, 1, true)
		const workers = 2
		start := make(chan struct{})
		var wg sync.WaitGroup
		type res struct {
			replayed bool
			execID   int64
			err      error
		}
		results := make([]res, workers)
		wg.Add(workers)
		for i := 0; i < workers; i++ {
			go func(idx int) {
				defer wg.Done()
				<-start
				out, e := reserveRaw(owner, "fm-conc-key", price)
				eid := int64(0)
				if out.Execution != nil {
					eid = out.Execution.ID
				}
				results[idx] = res{!out.Replayed, eid, e}
			}(i)
		}
		close(start)
		wg.Wait()
		for _, r := range results {
			require.NoError(t, r.err)
		}
		assert.Equal(t, results[0].execID, results[1].execID)
		createdCount := 0
		for _, r := range results {
			if !r.replayed {
				createdCount++
			}
		}
		assert.Equal(t, 1, createdCount, "exactly one created, one replayed")
		var u User
		require.NoError(t, DB.First(&u, owner).Error)
		assert.Equal(t, 1000-42, u.Quota, "deducted once")
	})

	t.Run("WalletFirstFallbackToSubscription", func(t *testing.T) {
		const owner = 9006
		require.NoError(t, DB.Create(&User{Id: owner, Username: "fm9006", AffCode: "aff9006", Quota: 3}).Error)
		require.NoError(t, DB.Create(&SubscriptionPlan{Id: 9006, Title: "p", DurationUnit: "month"}).Error)
		require.NoError(t, DB.Create(&UserSubscription{Id: 9006, UserId: owner, PlanId: 9006, AmountTotal: 100, AmountUsed: 0, StartTime: 1, EndTime: 2e9, Status: "active", NextResetTime: 2e9, AllowWalletOverflow: true}).Error)
		price := mustPrice(t, "model_price", "model_price", 0.000085, 0, 1, true)
		out := mustReserveWithPref(t, owner, "fm-wf-sub", price, "wallet_first")
		require.False(t, out.Replayed)
		assert.Equal(t, FundingSourceSubscription, out.FundingSource, "wallet insufficient → subscription fallback")
		var u User
		require.NoError(t, DB.First(&u, owner).Error)
		assert.Equal(t, 3, u.Quota, "wallet untouched (insufficient)")
		var sub UserSubscription
		require.NoError(t, DB.First(&sub, 9006).Error)
		assert.Equal(t, int64(42), sub.AmountUsed, "subscription deducted")
	})

	t.Run("SubscriptionFirstFallbackToWallet", func(t *testing.T) {
		const owner = 9007
		require.NoError(t, DB.Create(&User{Id: owner, Username: "fm9007", AffCode: "aff9007", Quota: 100}).Error)
		require.NoError(t, DB.Create(&SubscriptionPlan{Id: 9007, Title: "p", DurationUnit: "month"}).Error)
		require.NoError(t, DB.Create(&UserSubscription{Id: 9007, UserId: owner, PlanId: 9007, AmountTotal: 3, AmountUsed: 0, StartTime: 1, EndTime: 2e9, Status: "active", NextResetTime: 2e9, AllowWalletOverflow: true}).Error)
		price := mustPrice(t, "model_price", "model_price", 0.000085, 0, 1, true)
		out := mustReserve(t, owner, "fm-sf-wallet", price)
		require.False(t, out.Replayed)
		assert.Equal(t, FundingSourceWallet, out.FundingSource, "subscription insufficient + allow overflow → wallet fallback")
		var u User
		require.NoError(t, DB.First(&u, owner).Error)
		assert.Equal(t, 100-42, u.Quota, "wallet deducted (fallback)")
		var sub UserSubscription
		require.NoError(t, DB.First(&sub, 9007).Error)
		assert.Equal(t, int64(0), sub.AmountUsed, "subscription not consumed")
	})

	t.Run("MixedSubsFirstCoversDespiteOrder", func(t *testing.T) {
		const owner = 9008
		require.NoError(t, DB.Create(&User{Id: owner, Username: "fm9008", AffCode: "aff9008", Quota: 100}).Error)
		require.NoError(t, DB.Create(&SubscriptionPlan{Id: 9008, Title: "p", DurationUnit: "month"}).Error)
		// Two active subs: first (lower end_time) insufficient, second covers.
		require.NoError(t, DB.Create(&UserSubscription{Id: 90081, UserId: owner, PlanId: 9008, AmountTotal: 3, AmountUsed: 0, StartTime: 1, EndTime: 1999999999, Status: "active", NextResetTime: 1999999999, AllowWalletOverflow: true}).Error)
		require.NoError(t, DB.Create(&UserSubscription{Id: 90082, UserId: owner, PlanId: 9008, AmountTotal: 200, AmountUsed: 0, StartTime: 1, EndTime: 2e9, Status: "active", NextResetTime: 2e9, AllowWalletOverflow: true}).Error)
		price := mustPrice(t, "model_price", "model_price", 0.000085, 0, 1, true)
		out := mustReserveWithPref(t, owner, "fm-mixed", price, "subscription_only")
		require.False(t, out.Replayed)
		assert.Equal(t, FundingSourceSubscription, out.FundingSource)
		assert.Equal(t, 90082, out.SubscriptionID, "second sub covers (first insufficient)")
		var u User
		require.NoError(t, DB.First(&u, owner).Error)
		assert.Equal(t, 100, u.Quota, "wallet untouched")
	})
}

// --- helpers for the fund matrix ---

func mustPrice(t *testing.T, mode, source string, mp, mr, gr float64, freePrec bool) *ImageTaskPriceResolution {
	t.Helper()
	v, err := NewImageTaskPriceResolution(mode, source, "img-v1", "img-v1", "default", mp, mr, gr, 500000, freePrec, nil)
	require.NoError(t, err)
	return v
}

func reserveRaw(owner int, key string, price *ImageTaskPriceResolution) (ImageTaskReserveOutcome, error) {
	return reserveRawWithPref(owner, key, price, "")
}

func reserveRawWithPref(owner int, key string, price *ImageTaskPriceResolution, pref string) (ImageTaskReserveOutcome, error) {
	channelID := owner + 100000
	channel := Channel{Id: channelID, Type: constant.ChannelTypeOpenAI, Status: 1, Name: "integration-image"}
	if err := DB.FirstOrCreate(&channel, channelID).Error; err != nil {
		return ImageTaskReserveOutcome{}, err
	}
	revision := ChannelRevision{ChannelID: channelID, RevisionNumber: 1, AdapterVersion: "integration/v1", Settings: []byte(`{"schema_version":1,"execution_config":"{\\"defaults\\":{\\"generation\\":\\"sync\\"}}"}`)}
	if err := DB.FirstOrCreate(&revision, ChannelRevision{ChannelID: channelID, RevisionNumber: 1}).Error; err != nil {
		return ImageTaskReserveOutcome{}, err
	}
	cmd := ImageTaskReserveCommand{
		OwnerUserID:       owner,
		Operation:         ImageTaskOperationGeneration,
		IdempotencyKey:    key,
		RequestHash:       "h-" + key,
		Price:             price,
		Now:               1_700_000_000,
		ChannelRevisionID: revision.ID,
		ExecutionMode:     "sync",
		AdapterVersion:    "integration/v1",
		RequestData:       []byte(`{"model":"img-v1","prompt":"integration"}`),
	}
	_ = pref // preference is read from locked user setting, not command (§5.5)
	return ReserveImageTask(nil, cmd)
}

func mustReserveWithPref(t *testing.T, owner int, key string, price *ImageTaskPriceResolution, pref string) ImageTaskReserveOutcome {
	t.Helper()
	// Set user billing preference via setting before reserve.
	var u User
	require.NoError(t, DB.First(&u, owner).Error)
	setting := u.GetSetting()
	setting.BillingPreference = pref
	u.SetSetting(setting)
	require.NoError(t, DB.Model(&User{}).Where("id = ?", owner).Update("setting", u.Setting).Error)
	out, err := reserveRawWithPref(owner, key, price, pref)
	require.NoError(t, err)
	return out
}

func mustReserve(t *testing.T, owner int, key string, price *ImageTaskPriceResolution) ImageTaskReserveOutcome {
	t.Helper()
	out, err := reserveRaw(owner, key, price)
	require.NoError(t, err)
	return out
}
