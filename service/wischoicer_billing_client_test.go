package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeBillingJSON(t *testing.T, w http.ResponseWriter, status int, body interface{}) {
	t.Helper()
	w.WriteHeader(status)
	b, err := common.Marshal(body)
	require.NoError(t, err)
	_, _ = w.Write(b)
}

// newTestClient 起一个 mock billing 服务，返回直连它的客户端。tokenHeader 捕获收到的 Token A。
func newTestClient(t *testing.T, fn http.HandlerFunc, timeout time.Duration) (*httpBillingRechargeOrderClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(fn)
	t.Cleanup(srv.Close)
	return &httpBillingRechargeOrderClient{baseURL: srv.URL, token: "token-A", http: &http.Client{Timeout: timeout}}, srv
}

func TestBillingCreateOrder_MapsStatusAndCodes(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		env       billingEnvelope
		wantErr   error
		retryable bool
		wantOrder bool
	}{
		{
			name:      "success 200 returns order",
			status:    200,
			env:       billingEnvelope{Success: true, Data: &BillingOrder{OrderNo: "O1", AmountCents: 5000, Currency: "CNY", Status: "PENDING", CodeUrl: "wx://qr"}},
			wantOrder: true,
		},
		{
			name:      "success 202 recovery returns order",
			status:    202,
			env:       billingEnvelope{Success: true, Data: &BillingOrder{OrderNo: "O1", Status: "PENDING"}},
			wantOrder: true,
		},
		{"401 unauthorized", 401, billingEnvelope{Success: false, Code: "UNAUTHORIZED"}, ErrBillingUnauthorized, false, false},
		{"400 invalid_argument", 400, billingEnvelope{Success: false, Code: "INVALID_ARGUMENT"}, ErrBillingInvalidArgument, false, false},
		{"404 order_not_found", 404, billingEnvelope{Success: false, Code: "ORDER_NOT_FOUND"}, ErrBillingOrderNotFound, false, false},
		{"409 order_conflict", 409, billingEnvelope{Success: false, Code: "ORDER_CONFLICT"}, ErrBillingOrderConflict, false, false},
		{"409 quota_capacity_exceeded", 409, billingEnvelope{Success: false, Code: "QUOTA_CAPACITY_EXCEEDED"}, ErrBillingCapacityExceeded, false, false},
		{"409 credit_user_unavailable", 409, billingEnvelope{Success: false, Code: "CREDIT_USER_UNAVAILABLE"}, ErrBillingUserUnavailable, false, false},
		{"500 transient -> unavailable retryable", 500, billingEnvelope{Success: false, Code: "INTERNAL"}, ErrBillingUnavailable, true, false},
		{"429 transient -> unavailable retryable", 429, billingEnvelope{Success: false, Code: "RATE_LIMITED"}, ErrBillingUnavailable, true, false},
		{"unknown code -> unavailable retryable", 409, billingEnvelope{Success: false, Code: "SOMETHING_NEW"}, ErrBillingUnavailable, true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc := tc
			var gotToken string
			client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				gotToken = r.Header.Get("X-Internal-Service-Token")
				writeBillingJSON(t, w, tc.status, tc.env)
			}, time.Second)

			order, err := client.CreateOrder(context.Background(), BillingCreateOrderRequest{
				NewApiUserId: 42, ClientRequestId: "rid-1", AmountCents: 5000,
			})

			// Token A 始终随请求发出。
			assert.Equal(t, "token-A", gotToken)
			if tc.wantErr != nil {
				require.Error(t, err)
				assert.ErrorIs(t, err, tc.wantErr)
				assert.Equal(t, tc.retryable, IsBillingRetryable(err))
				assert.Nil(t, order)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, order)
			assert.Equal(t, tc.env.Data.OrderNo, order.OrderNo)
		})
	}
}

// TestBillingError_Leakage 断言 billing 回包里的敏感 message / token 绝不出现在面向用户的 error 文本里。
func TestBillingError_Leakage(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeBillingJSON(t, w, 409, billingEnvelope{
			Success: false,
			Code:    "ORDER_CONFLICT",
			Message: "SENSITIVE_INTERNAL_DETAIL quota=99000 user_email=leak@example.com",
		})
	}, time.Second)

	_, err := client.CreateOrder(context.Background(), BillingCreateOrderRequest{
		NewApiUserId: 42, ClientRequestId: "rid", AmountCents: 5000,
	})
	require.Error(t, err)
	msg := err.Error()
	assert.NotContains(t, msg, "SENSITIVE_INTERNAL_DETAIL")
	assert.NotContains(t, msg, "leak@example.com")
	assert.NotContains(t, msg, "token-A")
}

func TestBillingGetOrder_SuccessAndNotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/new-api/v1/recharge-orders/", func(w http.ResponseWriter, r *http.Request) {
		// 区分 ?newApiUserId= 查单（GET）与路径。
		if !strings.HasPrefix(r.URL.Path, "/internal/new-api/v1/recharge-orders/missing") {
			writeBillingJSON(t, w, 200, billingEnvelope{Success: true, Data: &BillingOrder{
				OrderNo: "O1", AmountCents: 10000, Currency: "CNY", Status: "PAID",
			}})
			return
		}
		writeBillingJSON(t, w, 404, billingEnvelope{Success: false, Code: "ORDER_NOT_FOUND"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := &httpBillingRechargeOrderClient{baseURL: srv.URL, token: "token-A", http: &http.Client{Timeout: time.Second}}

	order, err := client.GetOrder(context.Background(), "O1", 42)
	require.NoError(t, err)
	require.NotNil(t, order)
	assert.Equal(t, "O1", order.OrderNo)
	assert.Equal(t, int64(10000), order.AmountCents)
	assert.Equal(t, "PAID", order.Status)

	_, err = client.GetOrder(context.Background(), "missing", 42)
	assert.ErrorIs(t, err, ErrBillingOrderNotFound)
}

func TestBillingClient_FailClosedWhenUnconfigured(t *testing.T) {
	// 未配置 baseURL：绝不裸连，直接 fail-closed。
	emptyURL := &httpBillingRechargeOrderClient{baseURL: "", token: "token-A", http: &http.Client{Timeout: time.Second}}
	_, err := emptyURL.CreateOrder(context.Background(), BillingCreateOrderRequest{NewApiUserId: 42, ClientRequestId: "r", AmountCents: 5000})
	assert.ErrorIs(t, err, ErrBillingUnavailable)

	// 未配置 Token A：绝不匿名放行。
	emptyToken := &httpBillingRechargeOrderClient{baseURL: "http://127.0.0.1:1", token: "", http: &http.Client{Timeout: time.Second}}
	_, err = emptyToken.GetOrder(context.Background(), "O1", 42)
	assert.ErrorIs(t, err, ErrBillingUnavailable)
}

func TestBillingClient_TransportErrorIsRetryable(t *testing.T) {
	client, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {}, time.Second)
	srv.Close() // 关闭服务，触发 transport error。

	_, err := client.CreateOrder(context.Background(), BillingCreateOrderRequest{NewApiUserId: 42, ClientRequestId: "r", AmountCents: 5000})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBillingUnavailable)
	assert.True(t, IsBillingRetryable(err))
}

func TestBillingClient_TimeoutIsRetryable(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		writeBillingJSON(t, w, 200, billingEnvelope{Success: true, Data: &BillingOrder{OrderNo: "O1"}})
	}, 50*time.Millisecond)

	_, err := client.CreateOrder(context.Background(), BillingCreateOrderRequest{NewApiUserId: 42, ClientRequestId: "r", AmountCents: 5000})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBillingUnavailable)
	assert.True(t, IsBillingRetryable(err))
}
