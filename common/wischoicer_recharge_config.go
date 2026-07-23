package common

import (
	"fmt"
	"net"
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
	// 默认 = wischoicerMaxUserQuotaCap（¥1,000,000 对应值：QuotaPerUnit=500000，
	// $1=¥1，故 ¥1M = 5×10¹¹ 单位），覆盖重度测试账号退款积压（如 ~12.3B 单位 ≈ ¥24,600）
	// 且留足生产余量。user.quota 列已 bigint（WIS-561），余额存储走 int64；此软上限远低于
	// maxUserBalanceForStorage() 的 int64 物理硬界。env 配置不得突破 wischoicerMaxUserQuotaCap
	// ——若产品要放宽须另取 Jirui 点头（R2 P1：cap 必须锁住，不能由实现自行放行）。
	WischoicerMaxUserQuota int64 = wischoicerMaxUserQuotaCap

	// WischoicerBillingInternalServiceToken 持有解析后的 Token B（billing → new-api：
	// reserve / release / credit / GET 内部账务接口）。方向语义明确：仅 billing 持有
	// 并发送，new-api 仅用于验证这 4 个账务路由。解析顺序：先读方向语义主名
	// BILLING_TO_NEWAPI_SERVICE_TOKEN，为空再回退兼容别名 WISCHOICER_BILLING_INTERNAL_SERVICE_TOKEN。
	// 两个名字不得同时指向不同 secret；过渡期只允许显式 alias，不允许两向复用同一 secret。
	// 为空时 4 个内部账务路由不挂载（fail-closed）。
	WischoicerBillingInternalServiceToken = ""

	// WischoicerBillingInternalServiceTokenNext 是 Token B 的 next 槽（WIS-547 R3 已锁
	// 24h current/next 无损轮换）。接收端对 current 与 next 都做 constant-time 校验、
	// 不短路；轮换窗口内两个同方向 token 都被接受，撤销后旧 token 立即失效。next 可空
	//（未轮换时只认 current）。current 为空时路由不挂载（fail-closed），即便 next 非空。
	WischoicerBillingInternalServiceTokenNext = ""

	// WischoicerBillingInternalEnabled 标记 Token B 账务路由是否挂载（Token B 非空时 true）。
	WischoicerBillingInternalEnabled = false

	// NewApiToBillingServiceToken 持有解析后的 Token A current（new-api → billing：
	// /internal/new-api/v1/recharge-orders* 内部订单接口）。方向语义明确：仅 new-api
	// 持有并发送，billing 仅用于验证 internal order 路由。为空时钱包 façade 不挂载（fail-closed）。
	NewApiToBillingServiceToken = ""

	// NewApiToBillingServiceTokenNext 是 Token A 的 next 槽（WIS-547 R3 锁定的 current/next
	// 无损轮换，与 billing 侧 Token A 接收端 NewApiTrust 双槽对齐）。new-api 作为发送端始终
	// 发送 current；next 是轮换暂存槽：billing 接收端在轮换窗口内同时接受 current 与 next，
	// 故可先把 next 配到两端、再在发送端把 current 提升为原 next 值，全程不中断。next 可空
	//（未轮换时只发 current）；current 为权威：current 为空时即便 next 非空，钱包 façade 也不
	// 挂载（fail-closed，避免只配半边），与 Token B current/next 规则一致。
	NewApiToBillingServiceTokenNext = ""

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

// wischoicerMaxUserQuotaCap 是 WischoicerMaxUserQuota 的硬上限（也是默认值）：¥1,000,000
// 对应值（QuotaPerUnit=500000，$1=¥1，故 ¥1M = 5×10¹¹ 单位）。env 配置不得突破——
// 若产品要放宽须另取 Jirui 点头，不能由实现自行放行（R2 P1：cap 必须锁住）。
const wischoicerMaxUserQuotaCap int64 = 500_000_000_000

const (
	// Token B（billing → new-api）方向语义主名 + 兼容别名。
	EnvBillingToNewApiServiceToken     = "BILLING_TO_NEWAPI_SERVICE_TOKEN"
	EnvBillingToNewApiServiceTokenNext = "BILLING_TO_NEWAPI_SERVICE_TOKEN_NEXT"
	EnvWischoicerBillingInternalToken  = "WISCHOICER_BILLING_INTERNAL_SERVICE_TOKEN"
	// Token A（new-api → billing）方向语义主名 + next 槽。
	EnvNewApiToBillingServiceToken      = "NEWAPI_TO_BILLING_SERVICE_TOKEN"
	EnvNewApiToBillingServiceTokenNext  = "NEWAPI_TO_BILLING_SERVICE_TOKEN_NEXT"
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
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("%s must be an integer: %w", EnvWischoicerMaxUserQuota, err)
		}
		if parsed <= 0 || parsed > wischoicerMaxUserQuotaCap {
			return fmt.Errorf("%s must be in (0, %d], got %d", EnvWischoicerMaxUserQuota, wischoicerMaxUserQuotaCap, parsed)
		}
		WischoicerMaxUserQuota = parsed
	} else {
		// unset => 默认 cap（让 initWischoicerRechargeConfig 幂等：每次加载都把软上限
		// 重置为 cap 或 env 值，不残留前次 env 的值）。
		WischoicerMaxUserQuota = wischoicerMaxUserQuotaCap
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

	// Token B next 槽（current/next 双槽轮换结构，WIS-547 R3 已锁）。current 为权威：
	// current 为空时即便 next 非空也不挂载（fail-closed，避免只配半边）。
	WischoicerBillingInternalServiceTokenNext = os.Getenv(EnvBillingToNewApiServiceTokenNext)

	// Token A（new-api → billing）+ billing 基址：解析后决定钱包 façade 是否挂载。
	NewApiToBillingServiceToken = os.Getenv(EnvNewApiToBillingServiceToken)
	// Token A next 槽（current/next 双槽轮换结构，WIS-547 R3 已锁）。current 为权威：
	// current 为空时即便 next 非空，钱包 façade 也不挂载（fail-closed，见下），与 Token B 规则一致。
	NewApiToBillingServiceTokenNext = os.Getenv(EnvNewApiToBillingServiceTokenNext)
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

// validateWischoicerBillingBaseURL 校验 billing 基址（WIS-547 R3 拍板，A方案 sidecar-only）。
//
// 应用层只允许 loopback baseURL（localhost / 127.0.0.0/8 / ::1）：测试环境 billing +
// new-api 同机 loopback；生产由本机 sidecar 终止跨机 mTLS，应用只连本机 sidecar。
// 跨机地址（不论 http 还是 https）一律 fail-closed（启动拒绝 → 钱包路由不挂载）。
//
// 不拿 scheme==https 当 mTLS 证据：http.Client 没有 client cert / private CA /
// TLSClientConfig，https 只代表服务端 TLS，不满足双向身份；真 mTLS 由 sidecar 在部署
// 门禁完成并留证。这是把 R3 边界收紧（跨机一律 fail-closed），不是放宽。
//
// 不做 SSRF/私网过滤：billing 是受信内部服务，跨机私网可达性 + ACL 由部署侧负责（契约 §5）。
func validateWischoicerBillingBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https, got %q", u.Scheme)
	}
	host := u.Hostname() // 纯 hostname，已剥端口与 IPv6 方括号
	if host == "" {
		return fmt.Errorf("host must not be empty")
	}
	if !isLoopbackHost(host) {
		return fmt.Errorf("only loopback baseURL allowed (sidecar-only); cross-machine address must fail-closed: host=%q", host)
	}
	return nil
}

// isLoopbackHost 判定纯 hostname（无端口）是否为 loopback。只精确匹配 "localhost"，
// IP 走 net.ParseIP + IsLoopback——禁止用字符串前缀判断，避免 "127.evil.example" 这类
// 以 "127." 开头的外部 DNS 主机被误判为本机（R2 复审 P1-1）。
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
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
