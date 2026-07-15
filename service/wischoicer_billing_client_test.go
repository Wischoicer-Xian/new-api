package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestClient 构造一个直连 httptest server 的 client，backoff 极小以加速重试路径测试。
// httpClient 镜像生产构造（含禁止 redirect 的 CheckRedirect），让测试覆盖真实行为。
func newTestClient(t *testing.T, baseURL, token string) *httpBillingClient {
	t.Helper()
	return &httpBillingClient{
		baseURL: baseURL,
		token:   token,
		httpClient: &http.Client{
			Timeout: 2 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		maxRetries: 2,
		backoff:    time.Millisecond,
	}
}

func TestBillingClient_CreateOrder_Success_ParsesOrderAndSendsTokenA(t *testing.T) {
	var seenToken, seenPath, seenMethod string
	var seenBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenToken = r.Header.Get(billingInternalTokenHeader)
		seenPath = r.URL.Path
		seenMethod = r.Method
		seenBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"orderNo":"ORD123","amountCents":5000,"currency":"CNY","status":"PENDING","codeUrl":"weixin://wxpay/bizpayurl?pr=abc","expireTime":1700000000}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, "token-A-secret")
	order, err := c.CreateRechargeOrder(context.Background(), BillingCreateOrderRequest{
		NewApiUserID:    42,
		ClientRequestID: "crid-1",
		AmountCents:     5000,
	})
	require.NoError(t, err)
	require.NotNil(t, order)
	assert.Equal(t, "ORD123", order.OrderNo)
	assert.Equal(t, int64(5000), order.AmountCents)
	assert.Equal(t, "CNY", order.Currency)
	assert.Equal(t, "PENDING", order.Status)
	assert.Equal(t, "weixin://wxpay/bizpayurl?pr=abc", order.CodeURL)
	assert.Equal(t, int64(1700000000), order.ExpireTime)

	// Token A 随每次请求发送；路径与方法正确。
	assert.Equal(t, "token-A-secret", seenToken)
	assert.Equal(t, "/internal/new-api/v1/recharge-orders", seenPath)
	assert.Equal(t, http.MethodPost, seenMethod)

	// 请求体含 newApiUserId + clientRequestId + amountCents；clientRequestId 复用。
	var sent BillingCreateOrderRequest
	require.NoError(t, json.Unmarshal(seenBody, &sent))
	assert.Equal(t, 42, sent.NewApiUserID)
	assert.Equal(t, "crid-1", sent.ClientRequestID)
	assert.Equal(t, int64(5000), sent.AmountCents)
}

// TestBillingClient_SendsCurrentNotNext verifies the Token A dual-slot sender
// design (WIS-547): the client always sends current (token). next (tokenNext)
// is a staged rotation slot and is NEVER put on the wire — so the next value
// cannot leak via the header. Lossless rotation is guaranteed by the billing
// receiver accepting both during the window, not by the sender emitting next.
func TestBillingClient_SendsCurrentNotNext(t *testing.T) {
	var seenToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenToken = r.Header.Get(billingInternalTokenHeader)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"orderNo":"ORD","amountCents":5000,"currency":"CNY","status":"PENDING"}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, "cur-A")
	c.tokenNext = "nxt-A" // next slot staged
	_, err := c.CreateRechargeOrder(context.Background(), BillingCreateOrderRequest{
		NewApiUserID: 1, ClientRequestID: "crid", AmountCents: 5000,
	})
	require.NoError(t, err)
	assert.Equal(t, "cur-A", seenToken, "sender must send current; next must never be sent on the wire")
	assert.NotEqual(t, "nxt-A", seenToken, "next-slot value must not leak into the header")
}

func TestBillingClient_CreateOrder_OrderConflict_NotRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"success":false,"code":"ORDER_CONFLICT","message":"amount differs"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, "token-A")
	_, err := c.CreateRechargeOrder(context.Background(), BillingCreateOrderRequest{
		NewApiUserID: 1, ClientRequestID: "crid", AmountCents: 5000,
	})
	require.Error(t, err)
	var berr *BillingRechargeError
	require.ErrorAs(t, err, &berr)
	assert.Equal(t, http.StatusConflict, berr.HTTPStatus)
	assert.Equal(t, BillingErrOrderConflict, berr.Code)
	assert.False(t, berr.Retryable, "4xx business terminal must not retry")
}

func TestBillingClient_CreateOrder_QuotaCapacityExceeded_NotRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"success":false,"code":"QUOTA_CAPACITY_EXCEEDED"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, "token-A")
	_, err := c.CreateRechargeOrder(context.Background(), BillingCreateOrderRequest{
		NewApiUserID: 1, ClientRequestID: "crid", AmountCents: 5000,
	})
	var berr *BillingRechargeError
	require.ErrorAs(t, err, &berr)
	assert.Equal(t, BillingErrQuotaCapacityExceeded, berr.Code)
	assert.False(t, berr.Retryable)
}

func TestBillingClient_CreateOrder_Unauthorized_NotRetryable(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"success":false,"code":"UNAUTHORIZED"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, "token-A")
	_, err := c.CreateRechargeOrder(context.Background(), BillingCreateOrderRequest{
		NewApiUserID: 1, ClientRequestID: "crid", AmountCents: 5000,
	})
	var berr *BillingRechargeError
	require.ErrorAs(t, err, &berr)
	assert.Equal(t, BillingErrUnauthorized, berr.Code)
	assert.False(t, berr.Retryable)
	// 401 是终态，不重试：只调用一次。
	assert.Equal(t, int32(1), atomic.LoadInt32(&attempts), "UNAUTHORIZED must not be retried")
}

func TestBillingClient_CreateOrder_Retries5xxWithSameClientRequestID(t *testing.T) {
	var attempts int32
	var lastBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		lastBody, _ = io.ReadAll(r.Body)
		if atomic.LoadInt32(&attempts) < 2 {
			w.WriteHeader(http.StatusBadGateway) // 5xx → 结果未知，可重试
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"orderNo":"ORD-RETRY","amountCents":5000,"currency":"CNY","status":"PENDING"}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, "token-A")
	order, err := c.CreateRechargeOrder(context.Background(), BillingCreateOrderRequest{
		NewApiUserID: 7, ClientRequestID: "same-crid", AmountCents: 5000,
	})
	require.NoError(t, err)
	require.Equal(t, "ORD-RETRY", order.OrderNo)
	assert.GreaterOrEqual(t, atomic.LoadInt32(&attempts), int32(2), "must retry on 5xx")

	// 重试必须复用同一 clientRequestId（幂等键），绝不新建订单。
	var sent BillingCreateOrderRequest
	require.NoError(t, json.Unmarshal(lastBody, &sent))
	assert.Equal(t, "same-crid", sent.ClientRequestID)
	assert.Equal(t, 7, sent.NewApiUserID)
}

func TestBillingClient_CreateOrder_TransportErrorIsRetryable(t *testing.T) {
	// 一个立即关闭连接的 server，制造传输错误。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		require.True(t, ok)
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, "token-A")
	_, err := c.CreateRechargeOrder(context.Background(), BillingCreateOrderRequest{
		NewApiUserID: 1, ClientRequestID: "crid", AmountCents: 5000,
	})
	require.Error(t, err)
	var berr *BillingRechargeError
	require.ErrorAs(t, err, &berr)
	assert.True(t, berr.Retryable, "transport error must be retryable (result unknown)")
}

func TestBillingClient_CreateOrder_NonJSONResponseIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>bad gateway</html>")) // 非 JSON 空坏响应
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, "token-A")
	_, err := c.CreateRechargeOrder(context.Background(), BillingCreateOrderRequest{
		NewApiUserID: 1, ClientRequestID: "crid", AmountCents: 5000,
	})
	require.Error(t, err)
	var berr *BillingRechargeError
	require.ErrorAs(t, err, &berr)
	assert.True(t, berr.Retryable, "non-JSON response must be retryable")
}

func TestBillingClient_CreateOrder_InvalidArgumentsRejectedLocally(t *testing.T) {
	c := newTestClient(t, "http://example.internal", "token-A")
	cases := []BillingCreateOrderRequest{
		{NewApiUserID: 0, ClientRequestID: "crid", AmountCents: 5000},                   // user<=0
		{NewApiUserID: 1, ClientRequestID: "", AmountCents: 5000},                       // empty clientRequestId
		{NewApiUserID: 1, ClientRequestID: "crid", AmountCents: 0},                      // amount<=0
		{NewApiUserID: 1, ClientRequestID: string(make([]byte, 65)), AmountCents: 5000}, // too long
	}
	for _, tc := range cases {
		_, err := c.CreateRechargeOrder(context.Background(), tc)
		require.Error(t, err)
		var berr *BillingRechargeError
		require.ErrorAs(t, err, &berr)
		assert.Equal(t, BillingErrInvalidArgument, berr.Code)
	}
}

func TestBillingClient_GetOrder_RequiresOwnerFilter(t *testing.T) {
	var seenQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"orderNo":"ORD1","amountCents":5000,"currency":"CNY","status":"PAID","paidTime":1700000000}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, "token-A")
	order, err := c.GetRechargeOrder(context.Background(), "ORD1", 42)
	require.NoError(t, err)
	assert.Equal(t, "PAID", order.Status)
	// 查单必须带 newApiUserId 归属过滤。
	assert.Equal(t, "newApiUserId=42", seenQuery, "GET must carry newApiUserId for ownership filter")

	_, err = c.GetRechargeOrder(context.Background(), "", 42)
	require.Error(t, err)
	_, err = c.GetRechargeOrder(context.Background(), "ORD1", 0)
	require.Error(t, err)
}

func TestBillingClient_GetOrder_NotFound_NotRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"success":false,"code":"ORDER_NOT_FOUND"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, "token-A")
	_, err := c.GetRechargeOrder(context.Background(), "ORD1", 42)
	var berr *BillingRechargeError
	require.ErrorAs(t, err, &berr)
	assert.Equal(t, BillingErrOrderNotFound, berr.Code)
	assert.False(t, berr.Retryable)
}

func TestBillingClient_ListOrder_SendsOwnerAndLimit(t *testing.T) {
	var seenQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"items":[{"orderNo":"ORD1","amountCents":5000,"currency":"CNY","status":"PAID"}],"nextCursor":"c1"}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, "token-A")
	result, err := c.ListRechargeOrders(context.Background(), 42, "", 10)
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	assert.Equal(t, "c1", result.NextCursor)
	assert.Contains(t, seenQuery, "newApiUserId=42")
	assert.Contains(t, seenQuery, "limit=10")

	// limit 越界被钳制（>50 → 默认 20）。
	_, err = c.ListRechargeOrders(context.Background(), 42, "", 999)
	require.NoError(t, err)
	assert.Contains(t, seenQuery, "limit=20", "limit>50 must be clamped to default")
}

// P1-1（R2 复审）：billing 返回 3xx redirect 时，client 必须不跟随——redirect 目标主机
// 永远收不到请求，X-Internal-Service-Token（Token A）绝不泄露到 billing 之外。
func TestBillingClient_CreateOrder_RedirectNotFollowed_TokenNotLeaked(t *testing.T) {
	var targetHits int32
	var targetToken string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&targetHits, 1)
		targetToken = r.Header.Get(billingInternalTokenHeader)
		_, _ = w.Write([]byte(`{"success":true,"data":{}}`))
	}))
	defer target.Close()

	billing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound) // 302 → 外部主机
	}))
	defer billing.Close()

	c := newTestClient(t, billing.URL, "token-A-secret")
	_, err := c.CreateRechargeOrder(context.Background(), BillingCreateOrderRequest{
		NewApiUserID: 1, ClientRequestID: "crid", AmountCents: 5000,
	})
	require.Error(t, err)
	var berr *BillingRechargeError
	require.ErrorAs(t, err, &berr)
	assert.False(t, berr.Retryable, "redirect must not be retried")
	// 核心断言：redirect 目标零命中，Token A 没有被带过去。
	assert.Equal(t, int32(0), atomic.LoadInt32(&targetHits), "redirect target must receive NO request; Token A must not leak cross-host")
	assert.Empty(t, targetToken)
}
