package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// ---------------------------------------------------------------------------
// Wischoicer 钱包 → billing 受信内部订单客户端（Token A 方向，WIS-547 §1 / WIS-550）
//
// new-api 钱包 façade 以服务身份（Token A）调用 billing 的
// POST/GET /internal/new-api/v1/recharge-orders* 创建/查单。本客户端：
//   - 始终携带 X-Internal-Service-Token（Token A）；Token 绝不进日志、错误响应或 tracing；
//   - 解析 billing 的 {success,data} / {success:false,code,message} 包络；
//   - 把稳定 code 映射为类型化 error，供 controller 再映射为面向用户的安全文案；
//   - 429/5xx/超时/网络错误视为「结果未知」，返回可重试的 ErrBillingUnavailable；
//     调用方只用原 orderNo/clientRequestId 重试（幂等），绝不新建订单。
// ---------------------------------------------------------------------------

// BillingRechargeOrderClient 是钱包 façade 调用 billing 内部订单接口的抽象，便于单测注入桩。
type BillingRechargeOrderClient interface {
	// CreateOrder 创建或重取订单（幂等键 clientRequestId）。newApiUserId 由 new-api 钱包从
	// UserAuth context 派生，绝不来自浏览器。
	CreateOrder(ctx context.Context, req BillingCreateOrderRequest) (*BillingOrder, error)
	// GetOrder 按 orderNo 查单；billing 按 newApiUserId 过滤归属，跨用户返回 OrderNotFound。
	GetOrder(ctx context.Context, orderNo string, newApiUserId int) (*BillingOrder, error)
}

// BillingCreateOrderRequest 是 new-api → billing 创建订单的请求（契约 §1）。
type BillingCreateOrderRequest struct {
	NewApiUserId    int    `json:"newApiUserId"`
	ClientRequestId string `json:"clientRequestId"`
	AmountCents     int64  `json:"amountCents"`
}

// BillingOrder 是 billing 返回、经 new-api 钱包再投影给浏览器的订单视图。
// 仅含面向用户的安全字段；quota / 内部错误码 / 服务名一律不在此结构出现（结构性防泄漏）。
type BillingOrder struct {
	OrderNo     string `json:"orderNo"`
	AmountCents int64  `json:"amountCents"`
	Currency    string `json:"currency"`
	Status      string `json:"status"`
	CodeUrl     string `json:"codeUrl,omitempty"`
	ExpireTime  *int64 `json:"expireTime,omitempty"`
	PaidTime    *int64 `json:"paidTime,omitempty"`
}

// billing 调用 error。controller 据此映射为面向用户的安全文案；这些 error 本身不含 token / code / message。
var (
	// ErrBillingUnavailable：429/5xx/超时/网络/未知/未配置——结果未知，可用原幂等键安全重试。
	ErrBillingUnavailable      = errors.New("billing recharge unavailable")
	ErrBillingUnauthorized     = errors.New("billing recharge unauthorized")
	ErrBillingInvalidArgument  = errors.New("billing recharge invalid argument")
	ErrBillingOrderNotFound    = errors.New("billing recharge order not found")
	ErrBillingOrderConflict    = errors.New("billing recharge order conflict")
	ErrBillingCapacityExceeded = errors.New("billing recharge capacity exceeded")
	ErrBillingUserUnavailable  = errors.New("billing recharge user unavailable")
)

// IsBillingRetryable 报告该错误是否可用原 orderNo/clientRequestId 安全重试。
// 仅「结果未知」类（暂不可用）可重试；4xx 业务终态错误不重试。幂等键保证重试不重复入账。
func IsBillingRetryable(err error) bool {
	return errors.Is(err, ErrBillingUnavailable)
}

const (
	billingInternalTokenHeader = "X-Internal-Service-Token"
	billingRequestIDHeader     = "X-Request-Id"
	billingCreateOrderPath     = "/internal/new-api/v1/recharge-orders"
	billingGetOrderPathTmpl    = "/internal/new-api/v1/recharge-orders/%s?newApiUserId=%d"
	billingHTTPTimeout         = 10 * time.Second
	// billingMaxBody 限制读取的响应体大小，防御异常/恶意响应。
	billingMaxBody int64 = 1 << 20 // 1 MiB
)

// billingEnvelope 是 billing 内部接口的统一包络（契约 §0）。
type billingEnvelope struct {
	Success bool          `json:"success"`
	Code    string        `json:"code,omitempty"`
	Message string        `json:"message,omitempty"`
	Data    *BillingOrder `json:"data,omitempty"`
}

// httpBillingRechargeOrderClient 是 BillingRechargeOrderClient 的 net/http 实现。
type httpBillingRechargeOrderClient struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewBillingRechargeOrderClient 按 common 配置构造客户端。baseURL / Token A 任一为空时仍返回
// 客户端，但调用 fail-closed 返回 ErrBillingUnavailable（不 panic、不裸连、不匿名放行）。
func NewBillingRechargeOrderClient() BillingRechargeOrderClient {
	return &httpBillingRechargeOrderClient{
		baseURL: strings.TrimRight(common.WischoicerBillingBaseURL, "/"),
		token:   common.WischoicerNewApiToBillingToken,
		http:    &http.Client{Timeout: billingHTTPTimeout},
	}
}

// defaultBillingRechargeOrderClient 是懒加载的进程级客户端，供 controller 使用。
var defaultBillingRechargeOrderClient BillingRechargeOrderClient

// DefaultBillingRechargeOrderClient 返回（必要时构造）进程级默认客户端。
func DefaultBillingRechargeOrderClient() BillingRechargeOrderClient {
	if defaultBillingRechargeOrderClient == nil {
		defaultBillingRechargeOrderClient = NewBillingRechargeOrderClient()
	}
	return defaultBillingRechargeOrderClient
}

// SetBillingRechargeOrderClientForTest 供测试注入桩客户端；返回还原函数。
func SetBillingRechargeOrderClientForTest(c BillingRechargeOrderClient) func() {
	prev := defaultBillingRechargeOrderClient
	defaultBillingRechargeOrderClient = c
	return func() { defaultBillingRechargeOrderClient = prev }
}

func (c *httpBillingRechargeOrderClient) CreateOrder(ctx context.Context, req BillingCreateOrderRequest) (*BillingOrder, error) {
	body, err := common.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("%w: encode request", ErrBillingUnavailable)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+billingCreateOrderPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: build request", ErrBillingUnavailable)
	}
	c.setHeaders(httpReq, ctx, true)
	return c.do(httpReq)
}

func (c *httpBillingRechargeOrderClient) GetOrder(ctx context.Context, orderNo string, newApiUserId int) (*BillingOrder, error) {
	target := c.baseURL + fmt.Sprintf(billingGetOrderPathTmpl, url.PathEscape(orderNo), newApiUserId)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build request", ErrBillingUnavailable)
	}
	c.setHeaders(httpReq, ctx, false)
	return c.do(httpReq)
}

// setHeaders 写入 Token A 与公共 header。Token 仅出现在出站请求头，绝不落日志。
func (c *httpBillingRechargeOrderClient) setHeaders(req *http.Request, ctx context.Context, write bool) {
	req.Header.Set(billingInternalTokenHeader, c.token)
	req.Header.Set("Accept", "application/json")
	if write {
		req.Header.Set("Content-Type", "application/json")
	}
	if rid, ok := ctx.Value(common.RequestIdKey).(string); ok && rid != "" {
		req.Header.Set(billingRequestIDHeader, rid)
	}
}

func (c *httpBillingRechargeOrderClient) do(req *http.Request) (*BillingOrder, error) {
	if c.baseURL == "" || c.token == "" {
		// fail-closed：未配置 Token A / billing 地址，绝不裸连或匿名放行。
		return nil, ErrBillingUnavailable
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// 超时 / 网络 / DNS：结果未知，可安全重试。不把 err 文本拼进面向用户的 error。
		common.SysError("wischoicer wallet: billing transport error (path=" + req.URL.Path + ")")
		return nil, ErrBillingUnavailable
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, billingMaxBody))
	if err != nil {
		common.SysError("wischoicer wallet: billing read body error (status=" + resp.Status + ")")
		return nil, ErrBillingUnavailable
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrBillingUnauthorized
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		common.SysError("wischoicer wallet: billing transient failure (status=" + resp.Status + ")")
		return nil, ErrBillingUnavailable
	}

	var env billingEnvelope
	if err := common.Unmarshal(raw, &env); err != nil {
		common.SysError("wischoicer wallet: billing decode envelope error (status=" + resp.Status + ")")
		return nil, ErrBillingUnavailable
	}
	if !env.Success {
		return nil, mapBillingCode(env.Code)
	}
	if env.Data == nil {
		return nil, ErrBillingUnavailable
	}
	return env.Data, nil
}

// mapBillingCode 把 billing 稳定 code 映射为类型化 error。未知 code 一律归为可重试的
// ErrBillingUnavailable（幂等键保证安全），绝不把 code/message 透出给浏览器。
func mapBillingCode(code string) error {
	switch code {
	case "INVALID_ARGUMENT":
		return ErrBillingInvalidArgument
	case "ORDER_NOT_FOUND", "CREDIT_NOT_FOUND":
		return ErrBillingOrderNotFound
	case "ORDER_CONFLICT":
		return ErrBillingOrderConflict
	case "QUOTA_CAPACITY_EXCEEDED":
		return ErrBillingCapacityExceeded
	case "CREDIT_USER_UNAVAILABLE":
		return ErrBillingUserUnavailable
	case "UNAUTHORIZED":
		return ErrBillingUnauthorized
	default:
		common.SysError("wischoicer wallet: billing unknown error code: " + code)
		return ErrBillingUnavailable
	}
}
