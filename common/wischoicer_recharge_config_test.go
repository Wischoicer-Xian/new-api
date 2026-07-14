package common

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsWischoicerRechargeAmountAllowed_NormalTiers(t *testing.T) {
	// 默认无测试白名单。
	for _, amt := range []int64{5000, 10000, 20000, 50000} {
		assert.True(t, IsWischoicerRechargeAmountAllowed(amt, 9999), "tier %d allowed", amt)
	}
	for _, amt := range []int64{100, 4999, 5001, 1000, 0, -1, 500000} {
		assert.False(t, IsWischoicerRechargeAmountAllowed(amt, 9999), "non-tier %d rejected", amt)
	}
}

func TestIsWischoicerRechargeAmountAllowed_TestAmountOnlyForWhitelist(t *testing.T) {
	original := WischoicerRechargeTestUserIDs
	WischoicerRechargeTestUserIDs = map[int]struct{}{7: {}}
	t.Cleanup(func() { WischoicerRechargeTestUserIDs = original })

	assert.True(t, IsWischoicerRechargeAmountAllowed(WischoicerRechargeTestAmountCents, 7), "whitelist user can use ¥1 test path")
	assert.False(t, IsWischoicerRechargeAmountAllowed(WischoicerRechargeTestAmountCents, 8), "non-whitelist user cannot use ¥1")
}

func TestInitWischoicerRechargeConfig_TokenBPrimaryPreferredOverAlias(t *testing.T) {
	save := snapshotWischoicerConfig(t)
	t.Cleanup(save)

	// primary 与 alias 指向同一 secret： tolerated（过渡期等值 alias 不算冲突），以 primary 为准。
	require.NoError(t, os.Setenv(EnvBillingToNewApiServiceToken, "primary-B"))
	require.NoError(t, os.Setenv(EnvWischoicerBillingInternalToken, "primary-B"))
	t.Cleanup(func() {
		_ = os.Unsetenv(EnvBillingToNewApiServiceToken)
		_ = os.Unsetenv(EnvWischoicerBillingInternalToken)
	})

	require.NoError(t, initWischoicerRechargeConfig())
	assert.Equal(t, "primary-B", WischoicerBillingInternalServiceToken, "primary direction-semantic name is authoritative")
	assert.True(t, WischoicerBillingInternalEnabled)
}

func TestInitWischoicerRechargeConfig_TokenBAliasFallbackStillWorks(t *testing.T) {
	save := snapshotWischoicerConfig(t)
	t.Cleanup(save)

	require.NoError(t, os.Unsetenv(EnvBillingToNewApiServiceToken))
	require.NoError(t, os.Setenv(EnvWischoicerBillingInternalToken, "alias-B"))
	t.Cleanup(func() { _ = os.Unsetenv(EnvWischoicerBillingInternalToken) })

	require.NoError(t, initWischoicerRechargeConfig())
	assert.Equal(t, "alias-B", WischoicerBillingInternalServiceToken, "transitional alias still resolves Token B")
	assert.True(t, WischoicerBillingInternalEnabled)
}

func TestInitWischoicerRechargeConfig_TokenBConflictBetweenPrimaryAndAliasFails(t *testing.T) {
	save := snapshotWischoicerConfig(t)
	t.Cleanup(save)

	require.NoError(t, os.Setenv(EnvBillingToNewApiServiceToken, "primary-B"))
	require.NoError(t, os.Setenv(EnvWischoicerBillingInternalToken, "different-B"))
	t.Cleanup(func() {
		_ = os.Unsetenv(EnvBillingToNewApiServiceToken)
		_ = os.Unsetenv(EnvWischoicerBillingInternalToken)
	})

	err := initWischoicerRechargeConfig()
	require.Error(t, err, "primary and alias pointing at different secrets must fail-fast (no two-way reuse)")
}

func TestInitWischoicerRechargeConfig_TokenBEmptyFailClosed(t *testing.T) {
	save := snapshotWischoicerConfig(t)
	t.Cleanup(save)

	require.NoError(t, os.Unsetenv(EnvBillingToNewApiServiceToken))
	require.NoError(t, os.Unsetenv(EnvWischoicerBillingInternalToken))

	require.NoError(t, initWischoicerRechargeConfig())
	assert.False(t, WischoicerBillingInternalEnabled, "Token B empty => credit routes NOT mounted (fail-closed)")
}

func TestInitWischoicerRechargeConfig_WalletEnabledRequiresTokenAAndBaseURL(t *testing.T) {
	save := snapshotWischoicerConfig(t)
	t.Cleanup(save)

	// 两者齐备 → 钱包挂载。
	require.NoError(t, os.Setenv(EnvNewApiToBillingServiceToken, "token-A"))
	require.NoError(t, os.Setenv(EnvWischoicerBillingBaseURL, "http://billing.svc:8080"))
	t.Cleanup(func() {
		_ = os.Unsetenv(EnvNewApiToBillingServiceToken)
		_ = os.Unsetenv(EnvWischoicerBillingBaseURL)
	})
	require.NoError(t, initWischoicerRechargeConfig())
	assert.True(t, WischoicerWalletRechargeEnabled, "Token A + base URL => wallet mounted")

	// 缺 Token A → fail-closed。
	require.NoError(t, os.Unsetenv(EnvNewApiToBillingServiceToken))
	require.NoError(t, initWischoicerRechargeConfig())
	assert.False(t, WischoicerWalletRechargeEnabled, "missing Token A => wallet NOT mounted")
}

func TestInitWischoicerRechargeConfig_WalletRejectsInvalidBaseURL(t *testing.T) {
	save := snapshotWischoicerConfig(t)
	t.Cleanup(save)

	require.NoError(t, os.Setenv(EnvNewApiToBillingServiceToken, "token-A"))
	require.NoError(t, os.Setenv(EnvWischoicerBillingBaseURL, "ftp://bad-scheme"))
	t.Cleanup(func() {
		_ = os.Unsetenv(EnvNewApiToBillingServiceToken)
		_ = os.Unsetenv(EnvWischoicerBillingBaseURL)
	})

	err := initWischoicerRechargeConfig()
	require.Error(t, err, "non-http(s) base URL must fail-fast, not silently mount wallet")
}

func TestParseWischoicerRechargeTestUserIDs(t *testing.T) {
	assert.Empty(t, parseWischoicerRechargeTestUserIDs(""))
	assert.Empty(t, parseWischoicerRechargeTestUserIDs("   "))
	// 合法：1,2,3,7；abc/-5 被跳过（非致命）。
	m := parseWischoicerRechargeTestUserIDs("1, 2 ,3,abc,-5, 7")
	_, has1 := m[1]
	_, has2 := m[2]
	_, has3 := m[3]
	_, has7 := m[7]
	assert.True(t, has1 && has2 && has3 && has7, "valid ids parsed")
	assert.NotContains(t, m, 0, "non-numeric / non-positive entries skipped, not fatal")
	assert.Len(t, m, 4)
}

// snapshotWischoicerConfig 保存受测全局，返回恢复函数。config 在 InitEnv 一次性读取、
// 测试中需要反复 initWischoicerRechargeConfig，必须隔离彼此与进程默认值。
func snapshotWischoicerConfig(t *testing.T) func() {
	t.Helper()
	origTokenB := WischoicerBillingInternalServiceToken
	origEnabled := WischoicerBillingInternalEnabled
	origTokenA := NewApiToBillingServiceToken
	origBaseURL := WischoicerBillingBaseURL
	origWallet := WischoicerWalletRechargeEnabled
	origTest := WischoicerRechargeTestUserIDs
	return func() {
		WischoicerBillingInternalServiceToken = origTokenB
		WischoicerBillingInternalEnabled = origEnabled
		NewApiToBillingServiceToken = origTokenA
		WischoicerBillingBaseURL = origBaseURL
		WischoicerWalletRechargeEnabled = origWallet
		WischoicerRechargeTestUserIDs = origTest
	}
}
