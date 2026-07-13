package common

import (
	"fmt"
	"math"
	"os"
	"strconv"
)

// Wischoicer 充值容量预留 + 幂等入账相关配置。
//
// 这些变量在 InitEnv 中一次性读取，不支持热更新（方案 §3.2、§12.1）。
var (
	// WischoicerMaxUserQuota 是单个 new-api 用户的正余额 + 活跃预留总和上限。
	// 必须为正且不超过 math.MaxInt32。未配置时使用 MaxInt32（等价于不额外限制，
	// 保持原有行为）；显式配置为非法值（<=0 或 >MaxInt32）时启动 fail-fast。
	WischoicerMaxUserQuota = math.MaxInt32

	// WischoicerBillingInternalServiceToken 是 wischoicer-billing 调用 new-api
	// 内部接口（reserve/release/credit/GET）的共享 Token。为空时不注册内部路由。
	WischoicerBillingInternalServiceToken = ""

	// WischoicerBillingInternalEnabled 标记内部路由是否挂载（token 非空时 true）。
	WischoicerBillingInternalEnabled = false

	// WischoicerCacheSecondDeleteDelay 是入账后两阶段缓存删除的二次删除延迟（秒）。
	WischoicerCacheSecondDeleteDelay = 2

	// WischoicerCacheRetryInterval 是缓存删除后台扫描任务的执行间隔（秒）。
	WischoicerCacheRetryInterval = 60
)

const (
	EnvWischoicerMaxUserQuota           = "WISCHOICER_MAX_USER_QUOTA"
	EnvWischoicerBillingInternalToken   = "WISCHOICER_BILLING_INTERNAL_SERVICE_TOKEN"
	EnvWischoicerCacheSecondDeleteDelay = "WISCHOICER_CACHE_SECOND_DELETE_DELAY"
	EnvWischoicerCacheRetryInterval     = "WISCHOICER_CACHE_RETRY_INTERVAL"
)

// initWischoicerRechargeConfig 在 InitEnv 中调用，读取充值容量预留相关环境变量。
//
// 返回 error 而非直接 panic，让调用方（main）统一用 FatalLog 处理。
func initWischoicerRechargeConfig() error {
	if v := os.Getenv(EnvWischoicerMaxUserQuota); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("%s must be an integer: %w", EnvWischoicerMaxUserQuota, err)
		}
		if parsed <= 0 || parsed > math.MaxInt32 {
			return fmt.Errorf("%s must be positive and <= math.MaxInt32, got %d", EnvWischoicerMaxUserQuota, parsed)
		}
		WischoicerMaxUserQuota = parsed
	}

	WischoicerBillingInternalServiceToken = os.Getenv(EnvWischoicerBillingInternalToken)
	// Token 为空时不 fail-fast，改为条件注册：4 个内部路由不挂载（billing 调到
	// 返回 404）。这样现有 new-api 部署升级不受影响；billing 上线时配好 token
	// 重启 new-api，路由才生效。鉴权仍 fail-safe——空 token 时路由根本不存在。
	// 方案 §14 渐进上线要求 new-api 先能正常启动。
	if WischoicerBillingInternalServiceToken == "" {
		WischoicerBillingInternalEnabled = false
	} else {
		WischoicerBillingInternalEnabled = true
	}

	WischoicerCacheSecondDeleteDelay = GetEnvOrDefault(EnvWischoicerCacheSecondDeleteDelay, 2)
	if WischoicerCacheSecondDeleteDelay < 0 {
		WischoicerCacheSecondDeleteDelay = 2
	}

	WischoicerCacheRetryInterval = GetEnvOrDefault(EnvWischoicerCacheRetryInterval, 60)
	if WischoicerCacheRetryInterval <= 0 {
		WischoicerCacheRetryInterval = 60
	}

	return nil
}
