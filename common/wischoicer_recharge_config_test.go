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

	// 两者齐备 → 钱包挂载（测试环境同机 loopback 明文，符合 P1-1b 拓扑）。
	require.NoError(t, os.Setenv(EnvNewApiToBillingServiceToken, "token-A"))
	require.NoError(t, os.Setenv(EnvWischoicerBillingBaseURL, "http://127.0.0.1:8080"))
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
	origTokenBNext := WischoicerBillingInternalServiceTokenNext
	origEnabled := WischoicerBillingInternalEnabled
	origTokenA := NewApiToBillingServiceToken
	origTokenANext := NewApiToBillingServiceTokenNext
	origBaseURL := WischoicerBillingBaseURL
	origWallet := WischoicerWalletRechargeEnabled
	origTest := WischoicerRechargeTestUserIDs
	return func() {
		WischoicerBillingInternalServiceToken = origTokenB
		WischoicerBillingInternalServiceTokenNext = origTokenBNext
		WischoicerBillingInternalEnabled = origEnabled
		NewApiToBillingServiceToken = origTokenA
		NewApiToBillingServiceTokenNext = origTokenANext
		WischoicerBillingBaseURL = origBaseURL
		WischoicerWalletRechargeEnabled = origWallet
		WischoicerRechargeTestUserIDs = origTest
	}
}

// A方案（WIS-547 R3 拍板，sidecar-only）：应用层只允许 loopback baseURL（http 或 https）。
func TestValidateWischoicerBillingBaseURL_LoopbackAllowed(t *testing.T) {
	for _, u := range []string{
		"http://127.0.0.1:8080", "http://localhost:8080", "http://[::1]:8080",
		"http://127.1.2.3", "http://127.0.0.1", "http://127.255.255.255",
		"https://127.0.0.1", "https://localhost", "https://[::1]:8080",
	} {
		assert.NoError(t, validateWischoicerBillingBaseURL(u), "%s should be allowed (loopback)", u)
	}
}

// 非 loopback 一律 fail-closed——不论 http 还是 https（A方案：跨机交给 sidecar，应用不直连）。
func TestValidateWischoicerBillingBaseURL_NonLoopbackBannedAnyScheme(t *testing.T) {
	for _, u := range []string{
		"http://10.0.0.5:8080", "http://192.168.1.1", "http://billing.svc:8080", "http://billing.internal",
		"https://10.0.0.5", "https://billing.svc:443", "https://billing.internal",
	} {
		assert.Error(t, validateWischoicerBillingBaseURL(u), "%s should be rejected (non-loopback, any scheme)", u)
	}
}

// P1-1 回归（R2 复审）：禁止用字符串前缀判 loopback——以 "127." 开头的外部域名必须被拒。
func TestValidateWischoicerBillingBaseURL_LoopbackPrefixBypassRejected(t *testing.T) {
	for _, u := range []string{
		"http://127.evil.example:8080", "http://127.0.0.1.evil.example:8080",
		"http://127.evil.example", "https://127.0.0.1.evil.example",
	} {
		assert.Error(t, validateWischoicerBillingBaseURL(u), "%s must be rejected (prefix bypass)", u)
	}
}

// 非 loopback（含 https）必须 fail-closed：启动报错 + 钱包不挂载（A方案收紧，非放宽）。
func TestInitWischoicerRechargeConfig_WalletFailClosedOnNonLoopback(t *testing.T) {
	save := snapshotWischoicerConfig(t)
	t.Cleanup(save)

	require.NoError(t, os.Setenv(EnvNewApiToBillingServiceToken, "token-A"))
	t.Cleanup(func() {
		_ = os.Unsetenv(EnvNewApiToBillingServiceToken)
		_ = os.Unsetenv(EnvWischoicerBillingBaseURL)
	})

	for _, baseURL := range []string{"http://10.0.0.5:8080", "https://billing.svc:443"} {
		require.NoError(t, os.Setenv(EnvWischoicerBillingBaseURL, baseURL))
		err := initWischoicerRechargeConfig()
		require.Error(t, err, "non-loopback %s must fail-closed at startup (A方案)", baseURL)
		assert.False(t, WischoicerWalletRechargeEnabled, "wallet must NOT mount on non-loopback base URL")
	}
}

// Token B next 槽：current 为权威，next 可空。
func TestInitWischoicerRechargeConfig_TokenBNextSlot(t *testing.T) {
	save := snapshotWischoicerConfig(t)
	t.Cleanup(save)

	require.NoError(t, os.Setenv(EnvBillingToNewApiServiceToken, "cur"))
	require.NoError(t, os.Setenv(EnvBillingToNewApiServiceTokenNext, "nxt"))
	t.Cleanup(func() {
		_ = os.Unsetenv(EnvBillingToNewApiServiceToken)
		_ = os.Unsetenv(EnvBillingToNewApiServiceTokenNext)
	})

	require.NoError(t, initWischoicerRechargeConfig())
	assert.Equal(t, "cur", WischoicerBillingInternalServiceToken)
	assert.Equal(t, "nxt", WischoicerBillingInternalServiceTokenNext)
	assert.True(t, WischoicerBillingInternalEnabled)

	// 只有 next、current 为空 → fail-closed 不挂载（current 权威）。
	require.NoError(t, os.Unsetenv(EnvBillingToNewApiServiceToken))
	require.NoError(t, initWischoicerRechargeConfig())
	assert.False(t, WischoicerBillingInternalEnabled, "current empty but next set must NOT mount")
}

// Token A（new-api → billing 发送端）next 槽：current 为权威，next 可空。发送端始终发
// current；next 是轮换暂存槽。与 Token B current/next 规则一致（WIS-547 无损轮换）。
func TestInitWischoicerRechargeConfig_TokenANextSlot(t *testing.T) {
	save := snapshotWischoicerConfig(t)
	t.Cleanup(save)

	require.NoError(t, os.Setenv(EnvNewApiToBillingServiceToken, "cur-A"))
	require.NoError(t, os.Setenv(EnvNewApiToBillingServiceTokenNext, "nxt-A"))
	require.NoError(t, os.Setenv(EnvWischoicerBillingBaseURL, "http://127.0.0.1:8080"))
	t.Cleanup(func() {
		_ = os.Unsetenv(EnvNewApiToBillingServiceToken)
		_ = os.Unsetenv(EnvNewApiToBillingServiceTokenNext)
		_ = os.Unsetenv(EnvWischoicerBillingBaseURL)
	})

	require.NoError(t, initWischoicerRechargeConfig())
	assert.Equal(t, "cur-A", NewApiToBillingServiceToken)
	assert.Equal(t, "nxt-A", NewApiToBillingServiceTokenNext)
	assert.True(t, WischoicerWalletRechargeEnabled, "current + baseURL => wallet mounted (next is staged, not required)")

	// next 未配置（未轮换）→ 仅 current，仍挂载。
	require.NoError(t, os.Unsetenv(EnvNewApiToBillingServiceTokenNext))
	require.NoError(t, initWischoicerRechargeConfig())
	assert.Equal(t, "", NewApiToBillingServiceTokenNext, "next empty when unset")
	assert.True(t, WischoicerWalletRechargeEnabled, "current alone still mounts wallet")

	// 只有 next、current 为空 → fail-closed 不挂载（current 权威，避免只配半边）。
	require.NoError(t, os.Unsetenv(EnvNewApiToBillingServiceToken))
	require.NoError(t, os.Setenv(EnvNewApiToBillingServiceTokenNext, "nxt-A"))
	require.NoError(t, initWischoicerRechargeConfig())
	assert.False(t, WischoicerWalletRechargeEnabled, "current empty but next set must NOT mount wallet")
}
