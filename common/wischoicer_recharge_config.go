package common

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// Wischoicer 充值容量预留 + 幂等入账 + 钱包桥接相关配置。
//
// 这些变量在 InitEnv 中一次性读取，不支持热更新（方案 §3.2、§12.1）。
var (
	// WischoicerMaxUserQuota 是单个 new-api 用户的正余额 + 活跃预留总和上限。
	// 必须为正且不超过 math.MaxInt32。未配置时使用 MaxInt32（等价于不额外限制，
	// 保持原有行为）；显式配置为非法值（<=0 或 >MaxInt32）时启动 fail-fast。
	WischoicerMaxUserQuota = math.MaxInt32

	// WischoicerBillingInternalServiceToken 是 wischoicer-billing 调用 new-api
	// 内部接口（reserve/release/credit/GET）的共享 Token（Token B，billing→new-api 方向）。
	// 为空时不注册内部路由（fail-closed）。
	//
	// 优先读取方向语义明确的 BILLING_TO_NEWAPI_SERVICE_TOKEN；WISCHOICER_BILLING_INTERNAL_SERVICE_TOKEN
	// 作为兼容别名（过渡期）。Token B 绝不与 Token A（new-api→billing）复用同一 secret。
	WischoicerBillingInternalServiceToken = ""

	// WischoicerBillingInternalEnabled 标记 Token B 内部路由是否挂载（token 非空时 true）。
	WischoicerBillingInternalEnabled = false

	// WischoicerCacheSecondDeleteDelay 是入账后两阶段缓存删除的二次删除延迟（秒）。
	WischoicerCacheSecondDeleteDelay = 2

	// WischoicerCacheRetryInterval 是缓存删除后台扫描任务的执行间隔（秒）。
	WischoicerCacheRetryInterval = 60

	// ---- 钱包桥接（new-api → billing，Token A 方向，WIS-550 S2）----

	// WischoicerBillingBaseURL 是 billing 受信内部订单 API 的基地址（仅 loopback / 私网 + ACL+mTLS）。
	// 形如 http://127.0.0.1:10104 或 http://10.0.0.x:10104。为空时钱包无法创建/查询订单（fail-closed）。
	WischoicerBillingBaseURL = ""

	// WischoicerNewApiToBillingToken 是 new-api 调 billing 内部订单接口的服务 Token（Token A，
	// new-api→billing 方向）。仅 new-api 持有并发送；与 Token B 双向独立，绝不复用。为空时钱包
	// 创建/查询订单失败（fail-closed），绝不为联调改成匿名放行。
	WischoicerNewApiToBillingToken = ""

	// WischoicerWalletRechargeEnabled 是钱包充值 façade 的总开关。默认 false：/api/wallet/recharges*
	// 路由不挂载（fail-closed），浏览器完全不可达。需在 Token A/B、billing S3 端点、ACL/mTLS、
	// 真实小额 E2E 与 T+1 对账证据齐全后，经 R3（Jirui Zhao）批准才置 true。
	// 它与 billing 侧 BILLING_CREATE_ORDER_ENABLED 相互独立——两者都为 true 才真正可充值。
	WischoicerWalletRechargeEnabled = false

	// WischoicerRechargeTestUserIDs 是允许使用 ¥1（amountCents=100）受控测试档的 new-api 用户 ID 集合。
	// 默认空：任何用户提交 amountCents=100 都被服务端拒绝。仅用于测试环境 / 生产白名单测试账号。
	WischoicerRechargeTestUserIDs = map[int]struct{}{}
)

const (
	EnvWischoicerMaxUserQuota           = "WISCHOICER_MAX_USER_QUOTA"
	EnvWischoicerBillingInternalToken   = "WISCHOICER_BILLING_INTERNAL_SERVICE_TOKEN"
	EnvWischoicerBillingInternalTokenV2 = "BILLING_TO_NEWAPI_SERVICE_TOKEN"
	EnvWischoicerCacheSecondDeleteDelay = "WISCHOICER_CACHE_SECOND_DELETE_DELAY"
	EnvWischoicerCacheRetryInterval     = "WISCHOICER_CACHE_RETRY_INTERVAL"

	// 钱包桥接（Token A 方向）
	EnvWischoicerBillingBaseURL        = "WISCHOICER_BILLING_BASE_URL"
	EnvWischoicerNewApiToBillingToken  = "NEWAPI_TO_BILLING_SERVICE_TOKEN"
	EnvWischoicerWalletRechargeEnabled = "WISCHOICER_WALLET_RECHARGE_ENABLED"
	EnvWischoicerRechargeTestUserIDs   = "WISCHOICER_RECHARGE_TEST_USER_IDS"
)

// WischoicerRechargeTierCents 是普通用户可见、可创建订单的固定金额档（人民币分）：
// ¥50 / ¥100 / ¥200 / ¥500（R3 拍板，WIS-547）。前端只渲染这四档；不提供自定义金额。
// 这是服务端权威清单——前端提交的金额必须命中其中之一，否则拒绝。
var WischoicerRechargeTierCents = []int64{5000, 10000, 20000, 50000}

// WischoicerRechargeTestTierCents 是仅受控测试可达的 ¥1 档（人民币分）。
// 普通用户即使手工构造该值也被服务端拒绝；仅 WischoicerRechargeTestUserIDs 内的账号可用。
const WischoicerRechargeTestTierCents int64 = 100

// WischoicerRechargeClientRequestIDMaxLen 是钱包创建订单 clientRequestId 的最大长度（契约 §0：1–64 字符）。
const WischoicerRechargeClientRequestIDMaxLen = 64

// initWischoicerRechargeConfig 在 InitEnv 中调用，读取充值容量预留与钱包桥接相关环境变量。
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

	// Token B（billing→new-api）：优先方向语义命名，兼容旧别名。
	tokenB := os.Getenv(EnvWischoicerBillingInternalTokenV2)
	if tokenB == "" {
		tokenB = os.Getenv(EnvWischoicerBillingInternalToken)
	}
	WischoicerBillingInternalServiceToken = tokenB
	// Token 为空时不 fail-fast，改为条件注册：4 个内部路由不挂载（billing 调到
	// 返回 404）。这样现有 new-api 部署升级不受影响；billing 上线时配好 token
	// 重启 new-api，路由才生效。鉴权仍 fail-safe——空 token 时路由根本不存在。
	// 方案 §14 渐进上线要求 new-api 先能正常启动。
	if WischoicerBillingInternalServiceToken == "" {
		WischoicerBillingInternalEnabled = false
	} else {
		WischoicerBillingInternalEnabled = true
	}

	// 钱包桥接（Token A 方向）。这些配置非法值不致命：未配置时钱包调用 fail-closed，
	// 路由是否挂载取决于 WischoicerWalletRechargeEnabled。
	WischoicerBillingBaseURL = strings.TrimSpace(os.Getenv(EnvWischoicerBillingBaseURL))
	WischoicerNewApiToBillingToken = os.Getenv(EnvWischoicerNewApiToBillingToken)
	WischoicerWalletRechargeEnabled = GetEnvOrDefaultBool(EnvWischoicerWalletRechargeEnabled, false)
	WischoicerRechargeTestUserIDs = parseWischoicerRechargeTestUserIDs(os.Getenv(EnvWischoicerRechargeTestUserIDs))

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

// parseWischoicerRechargeTestUserIDs 解析逗号分隔的 new-api 用户 ID 白名单（¥1 受控测试档）。
// 非法条目跳过并记录，不致命。
func parseWischoicerRechargeTestUserIDs(raw string) map[int]struct{} {
	out := map[int]struct{}{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.Atoi(part)
		if err != nil || id <= 0 {
			SysError("wischoicer recharge: ignore invalid test user id: " + part)
			continue
		}
		out[id] = struct{}{}
	}
	return out
}

// IsWischoicerRechargeTestUser 报告该 new-api 用户是否在 ¥1 受控测试白名单内。
func IsWischoicerRechargeTestUser(userID int) bool {
	if userID <= 0 {
		return false
	}
	_, ok := WischoicerRechargeTestUserIDs[userID]
	return ok
}

// IsWischoicerRechargeAllowedAmountCents 报告该金额（人民币分）是否可创建订单。
// 普通用户仅限四档（¥50/100/200/500）；¥1（100 分）仅白名单测试账号可达。
// 这是服务端权威校验，前端不可绕过（WIS-547 §1、WIS-550）。
func IsWischoicerRechargeAllowedAmountCents(amountCents int64, userID int) bool {
	for _, tier := range WischoicerRechargeTierCents {
		if amountCents == tier {
			return true
		}
	}
	if amountCents == WischoicerRechargeTestTierCents && IsWischoicerRechargeTestUser(userID) {
		return true
	}
	return false
}
