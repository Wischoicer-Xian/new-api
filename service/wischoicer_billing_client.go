package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// ---------------------------------------------------------------------------
// Wischoicer billing Token A client — new-api → billing 受信内部订单接口
// ---------------------------------------------------------------------------
//
// 该 client 以「服务身份」调用 billing 的 /internal/new-api/v1/recharge-orders*
// 内部订单接口，携带 X-Internal-Service-Token（Token A）。它是钱包 UserAuth
// façade 的下游：浏览器永远拿不到 billing 地址、Token A、quota 或内部错误码——
// façade 只把 BillingRechargeOrder 的安全子集投影给浏览器。
//
// 关键不变量（WIS-547 契约 §1/§3）：
//   - 创建/重试一律复用同一 clientRequestId（幂等键），绝不新建订单。
//   - 查单/列表必须带 newApiUserId，由调用方从 UserAuth context 派生，绝不来自浏览器。
//   - 失败包络 {success:false,code,message}：业务方依赖 code，不解析 message；
//     message 不得透传给浏览器。
//   - 429/5xx/网络超时/空坏响应视为「结果未知」，可重试（同 orderNo/clientRequestId）；
//     4xx（401/400/404/409）为终态，不重试。

// BillingRechargeOrder 是 billing 内部订单接口返回给 new-api 的订单视图。
// 它只含浏览器可见的安全字段；billing 即便误返 quota，本结构也无 Quota 字段，
// 反序列化时被静默丢弃，不会回流到浏览器（契约 §1/§6）。
type BillingRechargeOrder struct {
	OrderNo     string `json:"orderNo"`
	AmountCents int64  `json:"amountCents"`
	Currency    string `json:"currency"`
	Status      string `json:"status"`
	CodeURL     string `json:"codeUrl,omitempty"`
	ExpireTime  int64  `json:"expireTime,omitempty"`
	PaidTime    int64  `json:"paidTime,omitempty"`
}

// BillingCreateOrderRequest 是创建/重取订单的请求体。NewApiUserID 由调用方
// 从 UserAuth context 派生；ClientRequestID 是钱包侧幂等键。
type BillingCreateOrderRequest struct {
	NewApiUserID    int    `json:"newApiUserId"`
	ClientRequestID string `json:"clientRequestId"`
	AmountCents     int64  `json:"amountCents"`
}

// BillingListResult 是订单历史/恢复列表结果。
type BillingListResult struct {
	Items      []BillingRechargeOrder `json:"items"`
	NextCursor string                 `json:"nextCursor,omitempty"`
}

// 稳定错误码（契约 §3）。仅用于内部判定与映射，绝不原样透传给浏览器。
const (
	BillingErrInvalidArgument       = "INVALID_ARGUMENT"
	BillingErrUnauthorized          = "UNAUTHORIZED"
	BillingErrOrderNotFound         = "ORDER_NOT_FOUND"
	BillingErrOrderConflict         = "ORDER_CONFLICT"
	BillingErrQuotaCapacityExceeded = "QUOTA_CAPACITY_EXCEEDED"
	BillingErrCreditUserUnavailable = "CREDIT_USER_UNAVAILABLE"
)

// BillingRechargeError 是 billing 内部订单接口的业务/传输错误。
//   - Code：billing 返回的稳定错误码（传输/解析错误时为空）。
//   - Message：billing 返回的 message，仅供日志/审计，绝不透传浏览器。
//   - Retryable：429/5xx/网络超时/空坏响应为 true；4xx 业务终态为 false。
type BillingRechargeError struct {
	HTTPStatus int
	Code       string
	Message    string
	Retryable  bool
}

func (e *BillingRechargeError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("billing recharge error: http=%d code=%s", e.HTTPStatus, e.Code)
	}
	return fmt.Sprintf("billing recharge error: http=%d (transport/parse)", e.HTTPStatus)
}

// WischoicerBillingClient 是 new-api 调 billing 内部订单接口的 client 契约。
// 接口化便于钱包 façade注入 mock 做单元测试（E2E 依赖 S3 端点就绪）。
type WischoicerBillingClient interface {
	CreateRechargeOrder(ctx context.Context, req BillingCreateOrderRequest) (*BillingRechargeOrder, error)
	GetRechargeOrder(ctx context.Context, orderNo string, newApiUserID int) (*BillingRechargeOrder, error)
	ListRechargeOrders(ctx context.Context, newApiUserID int, cursor string, limit int) (*BillingListResult, error)
}

// httpBillingClient 是 WischoicerBillingClient 的 HTTP 实现。
type httpBillingClient struct {
	baseURL    string
	token      string // Token A，绝不写入日志/响应/tracing
	httpClient *http.Client
	maxRetries int
	backoff    time.Duration
}

// NewWischoicerBillingClient 用解析后的 Token A + billing 基址构造 client。
// 仅在 WischoicerWalletRechargeEnabled 为 true（即 Token A 非空且基址合法）时调用。
func NewWischoicerBillingClient() WischoicerBillingClient {
	return &httpBillingClient{
		baseURL: common.WischoicerBillingBaseURL,
		token:   common.NewApiToBillingServiceToken,
		httpClient: &http.Client{
			Timeout: time.Duration(common.WischoicerBillingClientTimeoutSeconds) * time.Second,
		},
		maxRetries: 2,
		backoff:    150 * time.Millisecond,
	}
}

const (
	billingInternalTokenHeader = "X-Internal-Service-Token"
	billingCreateOrderPath     = "/internal/new-api/v1/recharge-orders"
	billingMaxClientRequestLen = 64
)

// CreateRechargeOrder 创建或重取订单。同一 (newApiUserId, clientRequestId, amountCents)
// 返回原订单（幂等）；同键不同金额由 billing 返回 ORDER_CONFLICT。
func (c *httpBillingClient) CreateRechargeOrder(ctx context.Context, req BillingCreateOrderRequest) (*BillingRechargeOrder, error) {
	if req.NewApiUserID <= 0 {
		return nil, &BillingRechargeError{HTTPStatus: 0, Code: BillingErrInvalidArgument, Retryable: false}
	}
	if req.ClientRequestID == "" || len(req.ClientRequestID) > billingMaxClientRequestLen {
		return nil, &BillingRechargeError{HTTPStatus: 0, Code: BillingErrInvalidArgument, Retryable: false}
	}
	if req.AmountCents <= 0 {
		return nil, &BillingRechargeError{HTTPStatus: 0, Code: BillingErrInvalidArgument, Retryable: false}
	}

	body, err := common.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal create order request: %w", err)
	}

	// 创建请求复用同一 clientRequestId 重试，billing 侧按 (newApiUserId,clientRequestId)
	// 幂等返回原订单，绝不新建。终态 4xx 不重试。
	var order *BillingRechargeOrder
	cErr := c.doWithRetry(ctx, true, "POST", billingCreateOrderPath, "", body, &order)
	if cErr != nil {
		return nil, cErr
	}
	return order, nil
}

// GetRechargeOrder 按 orderNo 查单，强制带 newApiUserID 做归属过滤（契约 §1）。
func (c *httpBillingClient) GetRechargeOrder(ctx context.Context, orderNo string, newApiUserID int) (*BillingRechargeOrder, error) {
	if orderNo == "" || newApiUserID <= 0 {
		return nil, &BillingRechargeError{HTTPStatus: 0, Code: BillingErrInvalidArgument, Retryable: false}
	}
	// 查单必须按 newApiUserId 过滤，防枚举别人订单（契约 §1：归属不一致统一 404）。
	q := url.Values{}
	q.Set("newApiUserId", strconv.Itoa(newApiUserID))
	var order *BillingRechargeOrder
	cErr := c.doWithRetry(ctx, true, "GET", billingCreateOrderPath+"/"+url.PathEscape(orderNo), q.Encode(), nil, &order)
	if cErr != nil {
		return nil, cErr
	}
	return order, nil
}

// ListRechargeOrders 按 newApiUserID 分页查单。limit 被钳制到 1–50（契约 §1）。
func (c *httpBillingClient) ListRechargeOrders(ctx context.Context, newApiUserID int, cursor string, limit int) (*BillingListResult, error) {
	if newApiUserID <= 0 {
		return nil, &BillingRechargeError{HTTPStatus: 0, Code: BillingErrInvalidArgument, Retryable: false}
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	q := url.Values{}
	q.Set("newApiUserId", strconv.Itoa(newApiUserID))
	q.Set("limit", strconv.Itoa(limit))
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	var result *BillingListResult
	cErr := c.doWithRetry(ctx, true, "GET", billingCreateOrderPath, q.Encode(), nil, &result)
	if cErr != nil {
		return nil, cErr
	}
	return result, nil
}

// billingEnvelope 是 billing 内部接口的统一包络。
type billingEnvelope struct {
	Success bool            `json:"success"`
	Code    string          `json:"code,omitempty"`
	Message string          `json:"message,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// doWithRetry 执行一次（带重试）HTTP 调用，解析 billing 包络。
//   - retryable=true 时，对 429/5xx/网络/超时/空坏响应按指数退避重试（同幂等键）。
//   - 4xx 业务终态不重试，直接作为 *BillingRechargeError 返回。
//   - into 必须是 *T（如 *BillingRechargeOrder 或 *BillingListResult）。
func (c *httpBillingClient) doWithRetry(ctx context.Context, retryable bool, method, path, rawQuery string, body []byte, into interface{}) error {
	fullURL := c.baseURL + path
	if rawQuery != "" {
		fullURL += "?" + rawQuery
	}

	var lastErr error
	attempts := 1
	if retryable {
		attempts += c.maxRetries
	}
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			// 指数退避：base * 2^(attempt-1)。同 clientRequestId/orderNo 重试，不新建。
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(c.backoff << (attempt - 1)):
			}
		}

		var bodyReader io.Reader
		if body != nil {
			bodyReader = bytes.NewReader(body)
		}
		httpReq, err := http.NewRequestWithContext(ctx, method, fullURL, bodyReader)
		if err != nil {
			return fmt.Errorf("build billing request: %w", err)
		}
		httpReq.Header.Set(billingInternalTokenHeader, c.token)
		httpReq.Header.Set("Accept", "application/json")
		if body != nil {
			httpReq.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient.Do(httpReq)
		if err != nil {
			lastErr = &BillingRechargeError{HTTPStatus: 0, Retryable: true, Message: err.Error()}
			if !retryable || ctx.Err() != nil {
				return lastErr
			}
			continue
		}

		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MiB 上限
		resp.Body.Close()
		if readErr != nil {
			lastErr = &BillingRechargeError{HTTPStatus: resp.StatusCode, Retryable: true, Message: readErr.Error()}
			if !retryable {
				return lastErr
			}
			continue
		}

		// 解析包络。
		var env billingEnvelope
		if unmarshalErr := common.Unmarshal(respBody, &env); unmarshalErr != nil {
			// 空坏响应：结果未知，可重试。
			lastErr = &BillingRechargeError{HTTPStatus: resp.StatusCode, Retryable: true, Message: "non-json response"}
			if !retryable {
				return lastErr
			}
			continue
		}

		if !env.Success {
			// 业务失败：按 HTTP 状态判定是否可重试。4xx 终态，429/5xx 可重试。
			berr := &BillingRechargeError{
				HTTPStatus: resp.StatusCode,
				Code:       env.Code,
				Message:    env.Message,
			}
			berr.Retryable = isBillingStatusRetryable(resp.StatusCode)
			// 缺失 code 时按 HTTP 状态推断稳定 code（防御：billing 应总是返回 code）。
			if berr.Code == "" {
				berr.Code = billingCodeForStatus(resp.StatusCode)
			}
			if berr.Retryable && retryable && ctx.Err() == nil {
				lastErr = berr
				continue
			}
			return berr
		}

		// 成功：解 data 到目标结构。data 为空（如 202 恢复中）时保持 into 原值。
		if len(env.Data) > 0 && into != nil {
			if err := common.Unmarshal(env.Data, into); err != nil {
				return &BillingRechargeError{HTTPStatus: resp.StatusCode, Retryable: false, Message: "decode data: " + err.Error()}
			}
		}
		return nil
	}
	return lastErr
}

// isBillingStatusRetryable 判定 HTTP 状态是否属于「结果未知/暂不可用」可重试类。
func isBillingStatusRetryable(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// billingCodeForStatus 在 billing 未返回 code 时按 HTTP 状态推断稳定 code。
func billingCodeForStatus(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return BillingErrUnauthorized
	case status == http.StatusBadRequest:
		return BillingErrInvalidArgument
	case status == http.StatusNotFound:
		return BillingErrOrderNotFound
	case status == http.StatusConflict:
		return BillingErrOrderConflict
	case isBillingStatusRetryable(status):
		return ""
	default:
		return ""
	}
}
