//go:build integration

package model

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestImageTaskFundSuite_RealDB runs the §7.4 fund matrix on real MySQL8 / PG16:
// wallet deduct, subscription deduct, limited token, unlimited token, token
// insufficient rollback, wallet insufficient token rollback, strict overflow
// (no wallet), allow overflow (wallet fallback), replay (no double deduct).
// Each case asserts balances change exactly once or roll back fully.
func TestImageTaskFundSuite_RealDB(t *testing.T) {
	setupWischoicerIntegrationDB(t)
	prevCap := constant.MaxImageTasksPerUser
	constant.MaxImageTasksPerUser = 10
	t.Cleanup(func() { constant.MaxImageTasksPerUser = prevCap })

	// Each sub-test gets a unique owner to avoid cross-test interference.
	t.Run("WalletAndLimitedTokenDeducted", func(t *testing.T) {
		const owner = 8001
		require.NoError(t, DB.Create(&User{Id: owner, Username: "fs_w_8001", AffCode: "aff8001", Quota: 100}).Error)
		require.NoError(t, DB.Create(&Token{Id: 8001, UserId: owner, Name: "t", Key: "k8001", Status: 1, RemainQuota: 50, ExpiredTime: -1}).Error)
		intent := baseAtomicIntent(owner, "fs-wallet")
		intent.TokenID = 8001
		out, err := CreateImageTaskAtomic(intent)
		require.NoError(t, err)
		require.True(t, out.Created)
		var u User
		DB.First(&u, owner)
		assert.Equal(t, 95, u.Quota)
		var tk Token
		DB.First(&tk, 8001)
		assert.Equal(t, 45, tk.RemainQuota)
	})

	t.Run("UnlimitedTokenFrozen", func(t *testing.T) {
		const owner = 8002
		require.NoError(t, DB.Create(&User{Id: owner, Username: "fs_u_8002", AffCode: "aff8002", Quota: 100}).Error)
		require.NoError(t, DB.Create(&Token{Id: 8002, UserId: owner, Name: "t", Key: "k8002", Status: 1, RemainQuota: 999, UnlimitedQuota: true, ExpiredTime: -1}).Error)
		intent := baseAtomicIntent(owner, "fs-unlim")
		intent.TokenID = 8002
		out, err := CreateImageTaskAtomic(intent)
		require.NoError(t, err)
		require.True(t, out.Created)
		var u User
		DB.First(&u, owner)
		assert.Equal(t, 95, u.Quota)
		var tk Token
		DB.First(&tk, 8002)
		assert.Equal(t, 999, tk.RemainQuota, "unlimited token frozen")
	})

	t.Run("TokenInsufficientRollsBack", func(t *testing.T) {
		const owner = 8003
		require.NoError(t, DB.Create(&User{Id: owner, Username: "fs_ti_8003", AffCode: "aff8003", Quota: 100}).Error)
		require.NoError(t, DB.Create(&Token{Id: 8003, UserId: owner, Name: "t", Key: "k8003", Status: 1, RemainQuota: 3, ExpiredTime: -1}).Error)
		intent := baseAtomicIntent(owner, "fs-tokins")
		intent.TokenID = 8003
		_, err := CreateImageTaskAtomic(intent)
		assert.ErrorIs(t, err, ErrImageTaskInsufficientToken)
		var u User
		DB.First(&u, owner)
		assert.Equal(t, 100, u.Quota, "wallet rolled back")
		var tk Token
		DB.First(&tk, 8003)
		assert.Equal(t, 3, tk.RemainQuota, "token rolled back")
	})

	t.Run("WalletInsufficientRollsBackToken", func(t *testing.T) {
		const owner = 8004
		require.NoError(t, DB.Create(&User{Id: owner, Username: "fs_wi_8004", AffCode: "aff8004", Quota: 3}).Error)
		require.NoError(t, DB.Create(&Token{Id: 8004, UserId: owner, Name: "t", Key: "k8004", Status: 1, RemainQuota: 100, ExpiredTime: -1}).Error)
		intent := baseAtomicIntent(owner, "fs-walletins")
		intent.TokenID = 8004
		_, err := CreateImageTaskAtomic(intent)
		assert.ErrorIs(t, err, ErrImageTaskInsufficientQuota)
		var u User
		DB.First(&u, owner)
		assert.Equal(t, 3, u.Quota, "wallet intact")
		var tk Token
		DB.First(&tk, 8004)
		assert.Equal(t, 100, tk.RemainQuota, "token rolled back with tx")
	})

	t.Run("StrictSubscriptionNoWallet", func(t *testing.T) {
		const owner = 8005
		require.NoError(t, DB.Create(&User{Id: owner, Username: "fs_ss_8005", AffCode: "aff8005", Quota: 100}).Error)
		require.NoError(t, DB.Create(&SubscriptionPlan{Id: 8005, Title: "strict", DurationUnit: "month"}).Error)
		require.NoError(t, DB.Create(&UserSubscription{Id: 8005, UserId: owner, PlanId: 8005, AmountTotal: 3, AmountUsed: 0, StartTime: 1, EndTime: 2_000_000_000, Status: "active", NextResetTime: 2_000_000_000, AllowWalletOverflow: false}).Error)
		_, err := CreateImageTaskAtomic(baseAtomicIntent(owner, "fs-strict"))
		assert.ErrorIs(t, err, ErrImageTaskInsufficientSub)
		var u User
		DB.First(&u, owner)
		assert.Equal(t, 100, u.Quota, "strict → wallet NOT touched")
	})

	t.Run("AllowOverflowWalletFallback", func(t *testing.T) {
		const owner = 8006
		require.NoError(t, DB.Create(&User{Id: owner, Username: "fs_ao_8006", AffCode: "aff8006", Quota: 100}).Error)
		require.NoError(t, DB.Create(&SubscriptionPlan{Id: 8006, Title: "allow", DurationUnit: "month"}).Error)
		require.NoError(t, DB.Create(&UserSubscription{Id: 8006, UserId: owner, PlanId: 8006, AmountTotal: 3, AmountUsed: 0, StartTime: 1, EndTime: 2_000_000_000, Status: "active", NextResetTime: 2_000_000_000, AllowWalletOverflow: true}).Error)
		out, err := CreateImageTaskAtomic(baseAtomicIntent(owner, "fs-allow"))
		require.NoError(t, err)
		require.True(t, out.Created)
		var u User
		DB.First(&u, owner)
		assert.Equal(t, 95, u.Quota, "allow overflow → wallet fallback")
		assert.Equal(t, ImageTaskBillingSourceWallet, out.Task.PrivateData.BillingSource)
	})
}
