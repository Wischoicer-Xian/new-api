package common

import (
	"fmt"
	"math"
	"net/url"
	"os"
	"sort"
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

	// WischoicerBillingInternalServiceToken 持有解析后的 Token B（billing → new-api：
	// reserve / release / credit / GET 内部账务接口）。方向语义明确：仅 billing 持有
	// 并发送，new-api 仅用于验证这 4 个账务路由。解析顺序：先读方向语义主名
	// BILLING_TO_NEWAPI_SERVICE_TOKEN，为空再回退兼容别名 WISCHOICER_BILLING_INTERNAL_SERVICE_TOKEN。
	// 两个名字不得同时指向不同 secret；过渡期只允许显式 alias，不允许两向复用同一 secret。
	// 为空时 4 个内部账务路由不挂载（fail-closed）。
	WischoicerBillingInternalServiceToken = ""

	// WischoicerBillingInternalEnabled 标记 Token B 账务路由是否挂载（Token B 非空时 true）。
	WischoicerBillingInternalEnabled = false

	// NewApiToBillingServiceToken 持有解析后的 Token A（new-api → billing：
	// /internal/new-api/v1/recharge-orders* 内部订单接口）。方向语义明确：仅 new-api
	// 持有并发送，billing 仅用于验证 internal order 路由。为空时钱包 façade 不挂载（fail-closed）。
	NewApiToBillingServiceToken = ""

	// WischoicerBillingBaseURL 是 billing 内部订单 API 的基址（如
	// http://billing-internal.svc.cluster.local:8080）。仅供 new-api 服务身份调用，
	// 不得出现在前端构建变量或浏览器可达地址中。为空或非法时钱包 façade 不挂载。
	WischoicerBillingBaseURL = ""

	// WischoicerWalletRechargeEnabled 标记钱包 UserAuth façade 是否挂载：
	// 仅当 Token A 非空且 billing 基址合法时为 true（fail-closed）。Token B 账务路由
	// 与钱包 façade 各自独立 fail-closed，互不为联调放开匿名。
	WischoicerWalletRechargeEnabled = false

	// WischoicerRechargeTestUserIDs 是允许走 ¥1（amountCents=100）测试授权路径的
	// new-api 用户 ID 白名单。仅服务端读取，前端不可改金额/quota/用户归属。
	WischoicerRechargeTestUserIDs = map[int]struct{}{}

	// WischoicerBillingClientTimeoutSeconds 是 new-api 调 billing 内部订单 API 的 HTTP 超时（秒）。
	WischoicerBillingClientTimeoutSeconds = 10

	// WischoicerCacheSecondDeleteDelay 是入账后两阶段缓存删除的二次删除延迟（秒）。
	WischoicerCacheSecondDeleteDelay = 2

	// WischoicerCacheRetryInterval 是缓存删除后台扫描任务的执行间隔（秒）。
	WischoicerCacheRetryInterval = 60
)

const (
	// Token B（billing → new-api）方向语义主名 + 兼容别名。
	EnvBillingToNewApiServiceToken    = "BILLING_TO_NEWAPI_SERVICE_TOKEN"
	EnvWischoicerBillingInternalToken = "WISCHOICER_BILLING_INTERNAL_SERVICE_TOKEN"
	// Token A（new-api → billing）方向语义主名。
	EnvNewApiToBillingServiceToken      = "NEWAPI_TO_BILLING_SERVICE_TOKEN"
	EnvWischoicerBillingBaseURL         = "WISCHOICER_BILLING_BASE_URL"
	EnvWischoicerRechargeTestUserIDs    = "WISCHOICER_RECHARGE_TEST_USER_IDS"
	EnvWischoicerBillingClientTimeout   = "WISCHOICER_BILLING_CLIENT_TIMEOUT_SECONDS"
	EnvWischoicerMaxUserQuota           = "WISCHOICER_MAX_USER_QUOTA"
	EnvWischoicerCacheSecondDeleteDelay = "WISCHOICER_CACHE_SECOND_DELETE_DELAY"
	EnvWischoicerCacheRetryInterval     = "WISCHOICER_CACHE_RETRY_INTERVAL"
)

// WischoicerRechargeAllowedAmountCents 是钱包对普通用户开放的固定人民币档位（分）：
// ¥50 / ¥100 / ¥200 / ¥500。金额档位是策略锁定值（WIS-547 R3 决策点 3），
// 不可由前端或环境变量改变；¥1（100 分）仅服务端白名单测试账号路径可达
// （见 WischoicerRechargeTestAmountCents）。
var WischoicerRechargeAllowedAmountCents = []int64{5000, 10000, 20000, 50000}

// WischoicerRechargeTestAmountCents 是 ¥1 测试授权金额（分）。仅对
// WischoicerRechargeTestUserIDs 中的用户开放，用于服务端测试授权路径。
const WischoicerRechargeTestAmountCents int64 = 100

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

	// Token B（billing → new-api）：主名优先，别名回退。两者均为空时 fail-closed 不挂载。
	tokenBPrimary := os.Getenv(EnvBillingToNewApiServiceToken)
	tokenBAlias := os.Getenv(EnvWischoicerBillingInternalToken)
	switch {
	case tokenBPrimary != "" && tokenBAlias != "" && tokenBPrimary != tokenBAlias:
		// 两向复用/冲突保护：主名与别名同时配置但指向不同 secret，拒绝启动，避免静默用错 token。
		return fmt.Errorf("%s and alias %s are both set but differ; set only the primary direction-semantic name",
			EnvBillingToNewApiServiceToken, EnvWischoicerBillingInternalToken)
	case tokenBPrimary != "":
		WischoicerBillingInternalServiceToken = tokenBPrimary
	default:
		WischoicerBillingInternalServiceToken = tokenBAlias
	}
	// Token 为空时不 fail-fast，改为条件注册：4 个内部路由不挂载（billing 调到
	// 返回 404）。这样现有 new-api 部署升级不受影响；billing 上线时配好 token
	// 重启 new-api，路由才生效。鉴权仍 fail-safe——空 token 时路由根本不存在。
	// 方案 §14 渐进上线要求 new-api 先能正常启动。
	WischoicerBillingInternalEnabled = WischoicerBillingInternalServiceToken != ""

	// Token A（new-api → billing）+ billing 基址：解析后决定钱包 façade 是否挂载。
	NewApiToBillingServiceToken = os.Getenv(EnvNewApiToBillingServiceToken)
	WischoicerBillingBaseURL = strings.TrimRight(os.Getenv(EnvWischoicerBillingBaseURL), "/")
	if NewApiToBillingServiceToken != "" && WischoicerBillingBaseURL != "" {
		if err := validateWischoicerBillingBaseURL(WischoicerBillingBaseURL); err != nil {
			return fmt.Errorf("%s invalid: %w", EnvWischoicerBillingBaseURL, err)
		}
		WischoicerWalletRechargeEnabled = true
	} else {
		// 任一缺失：fail-closed，钱包 façade 不挂载（浏览器调到 404）。
		WischoicerWalletRechargeEnabled = false
	}

	WischoicerRechargeTestUserIDs = parseWischoicerRechargeTestUserIDs(os.Getenv(EnvWischoicerRechargeTestUserIDs))

	WischoicerBillingClientTimeoutSeconds = GetEnvOrDefault(EnvWischoicerBillingClientTimeout, 10)
	if WischoicerBillingClientTimeoutSeconds <= 0 {
		WischoicerBillingClientTimeoutSeconds = 10
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

// validateWischoicerBillingBaseURL 只做最小结构校验（scheme + host）。
// 不做 SSRF/私网过滤：billing 是受信内部服务，跨机私网可达性 + ACL 由部署侧负责
// （WIS-547 契约 §5）。这里拒绝明显非法值，避免拼出错误 URL 后泄露到日志或前端。
func validateWischoicerBillingBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("host must not be empty")
	}
	return nil
}

// parseWischoicerRechargeTestUserIDs 解析逗号分隔的用户 ID 白名单。非法项被跳过
// （不 fail-fast，避免一个坏值阻塞启动）；空字符串返回空 map。
func parseWischoicerRechargeTestUserIDs(raw string) map[int]struct{} {
	out := map[int]struct{}{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out
	}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.Atoi(part)
		if err != nil || id <= 0 {
			continue
		}
		out[id] = struct{}{}
	}
	return out
}

// IsWischoicerRechargeAmountAllowed 判断 amountCents 是否对该用户开放。
//
// 普通用户只能选 ¥50/¥100/¥200/¥500 四档；¥1（100 分）仅当用户在服务端测试白名单中
// 才可达（测试授权路径）。前端无法改金额——这是服务端权威门禁，绕过前端直接构造
// 请求（如 amountCents=100 的普通用户、或 amountCents=3000 的越界金额）一律被拒。
func IsWischoicerRechargeAmountAllowed(amountCents int64, userID int) bool {
	if amountCents == WischoicerRechargeTestAmountCents {
		_, ok := WischoicerRechargeTestUserIDs[userID]
		return ok
	}
	// WischoicerRechargeAllowedAmountCents 已升序；二分查找判定。
	idx := sort.Search(len(WischoicerRechargeAllowedAmountCents), func(i int) bool {
		return WischoicerRechargeAllowedAmountCents[i] >= amountCents
	})
	return idx < len(WischoicerRechargeAllowedAmountCents) && WischoicerRechargeAllowedAmountCents[idx] == amountCents
}
