package controller

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockBillingClient 记录 façade 下发的请求，返回预设订单/错误，用于隔离 billing（E2E 依赖 S3）。
type mockBillingClient struct {
	lastCreateReq  *service.BillingCreateOrderRequest
	lastGetOrderNo string
	lastGetUser    int
	lastListUser   int
	lastListLimit  int
	order          *service.BillingRechargeOrder
	listResult     *service.BillingListResult
	err            error
}

func (m *mockBillingClient) CreateRechargeOrder(ctx context.Context, req service.BillingCreateOrderRequest) (*service.BillingRechargeOrder, error) {
	m.lastCreateReq = &req
	return m.order, m.err
}
func (m *mockBillingClient) GetRechargeOrder(ctx context.Context, orderNo string, newApiUserID int) (*service.BillingRechargeOrder, error) {
	m.lastGetOrderNo = orderNo
	m.lastGetUser = newApiUserID
	return m.order, m.err
}
func (m *mockBillingClient) ListRechargeOrders(ctx context.Context, newApiUserID int, cursor string, limit int) (*service.BillingListResult, error) {
	m.lastListUser = newApiUserID
	m.lastListLimit = limit
	return m.listResult, m.err
}

func injectMockBillingClient(t *testing.T, m service.WischoicerBillingClient) {
	t.Helper()
	// 注入与恢复都走写锁：与生产惰性初始化共用同一把锁，不留并发读写口子。
	wischoicerBillingClientMu.Lock()
	original := wischoicerBillingClientInstance
	wischoicerBillingClientInstance = m
	wischoicerBillingClientMu.Unlock()
	t.Cleanup(func() {
		wischoicerBillingClientMu.Lock()
		wischoicerBillingClientInstance = original
		wischoicerBillingClientMu.Unlock()
	})
}

// TestWalletBillingClient_ConcurrentGetterIsRaceFree 回归 WIS-550 R3：并发首次调用
// getWischoicerBillingClient 必须只创建一个实例、无数据竞争（go test -race 必须通过）。
// 原实现对包级 interface 做无同步惰性初始化，64 并发即触发 :36 读 / :39 写竞争。
func TestWalletBillingClient_ConcurrentGetterIsRaceFree(t *testing.T) {
	// 从 nil 开始，强制走惰性初始化路径（正是原 bug 命中的并发读写处）。
	injectMockBillingClient(t, nil)

	const n = 64
	results := make([]service.WischoicerBillingClient, n)
	var wg sync.WaitGroup
	wg.Add(n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			results[i] = getWischoicerBillingClient()
		}()
	}
	close(start)
	wg.Wait()

	// 所有并发调用者观察到同一个非 nil 实例（只初始化一次）。
	require.NotNil(t, results[0])
	for i := 1; i < n; i++ {
		require.Same(t, results[0], results[i], "goroutine %d observed a different client", i)
	}
}

func callCreate(t *testing.T, userID int, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("id", userID)
	c.Request = httptest.NewRequest("POST", "/api/wallet/recharges", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	CreateWischoicerWalletRecharge(c)
	return w
}

// 核心安全断言：newApiUserID 只来自 UserAuth context，body 里的伪造 newApiUserId 被忽略。
func TestWalletCreate_NewApiUserIDOnlyFromContext_BodyForgedIDIgnored(t *testing.T) {
	mock := &mockBillingClient{order: &service.BillingRechargeOrder{
		OrderNo: "ORD1", AmountCents: 5000, Currency: "CNY", Status: "PENDING", CodeURL: "weixin://x",
	}}
	injectMockBillingClient(t, mock)

	// body 伪造 newApiUserId=999 与 user_id=999；真实 context 用户是 42。
	w := callCreate(t, 42, `{"amountCents":5000,"newApiUserId":999,"user_id":999,"clientRequestId":"crid-1"}`)
	require.Equal(t, 200, w.Code, w.Body.String())

	// 下游收到的是 context 的 42，不是 body 的 999。
	require.NotNil(t, mock.lastCreateReq)
	assert.Equal(t, 42, mock.lastCreateReq.NewApiUserID, "newApiUserID must come from UserAuth context, never body")
	assert.Equal(t, "crid-1", mock.lastCreateReq.ClientRequestID)
}

// 安全投影：响应只含安全字段，绝不含 quota/内部 code/token。
func TestWalletCreate_ProjectsOnlySafeFields(t *testing.T) {
	mock := &mockBillingClient{order: &service.BillingRechargeOrder{
		OrderNo: "ORD1", AmountCents: 5000, Currency: "CNY", Status: "PENDING", CodeURL: "weixin://x", ExpireTime: 1700000000, PaidTime: 1700000500,
	}}
	injectMockBillingClient(t, mock)

	w := callCreate(t, 42, `{"amountCents":5000}`)
	require.Equal(t, 200, w.Code, w.Body.String())

	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			OrderNo     string `json:"orderNo"`
			AmountCents int64  `json:"amountCents"`
			Currency    string `json:"currency"`
			Status      string `json:"status"`
			CodeURL     string `json:"codeUrl"`
			ExpireTime  int64  `json:"expireTime"`
			PaidTime    int64  `json:"paidTime"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Success)
	assert.Equal(t, "ORD1", resp.Data.OrderNo)
	assert.Equal(t, int64(5000), resp.Data.AmountCents)
	assert.Equal(t, "weixin://x", resp.Data.CodeURL)
	// paidTime 按契约 §1 透传给浏览器（支付时间）。
	assert.Equal(t, int64(1700000500), resp.Data.PaidTime)

	// 绝不出现 quota / token / 服务名 / 内部错误码。
	lower := strings.ToLower(w.Body.String())
	assert.NotContains(t, lower, "quota")
	assert.NotContains(t, lower, "token")
	assert.NotContains(t, lower, "billing")
}

// 金额档位服务端门禁：¥50/100/200/500 对普通用户开放；越界金额（¥30/¥500000）被拒。
func TestWalletCreate_AmountTierEnforced_NormalUser(t *testing.T) {
	mock := &mockBillingClient{order: &service.BillingRechargeOrder{OrderNo: "ORD", AmountCents: 0, Currency: "CNY", Status: "PENDING"}}
	injectMockBillingClient(t, mock)
	// 默认无测试白名单。

	allowed := []int64{5000, 10000, 20000, 50000}
	for _, amt := range allowed {
		w := callCreate(t, 42, `{"amountCents":`+strconv.FormatInt(amt, 10)+`}`)
		assert.Equal(t, 200, w.Code, "amount %d should be allowed", amt)
	}

	rejected := []int64{100, 3000, 4999, 50001, 1000, 500000}
	for _, amt := range rejected {
		mock.lastCreateReq = nil
		w := callCreate(t, 42, `{"amountCents":`+strconv.FormatInt(amt, 10)+`}`)
		assert.Equal(t, 400, w.Code, "amount %d should be rejected", amt)
		assert.Nil(t, mock.lastCreateReq, "rejected amount must not reach billing")
	}
}

// ¥1（amountCents=100）仅服务端白名单测试用户可达；普通用户被拒。
func TestWalletCreate_AmountTier_TestAmountOnlyForWhitelist(t *testing.T) {
	original := common.WischoicerRechargeTestUserIDs
	common.WischoicerRechargeTestUserIDs = map[int]struct{}{7: {}}
	t.Cleanup(func() { common.WischoicerRechargeTestUserIDs = original })

	mock := &mockBillingClient{order: &service.BillingRechargeOrder{OrderNo: "ORD", AmountCents: 100, Currency: "CNY", Status: "PENDING"}}
	injectMockBillingClient(t, mock)

	// 普通用户 42 请求 ¥1 → 拒绝。
	w := callCreate(t, 42, `{"amountCents":100}`)
	assert.Equal(t, 400, w.Code)
	assert.Nil(t, mock.lastCreateReq)

	// 白名单用户 7 请求 ¥1 → 放行。
	w = callCreate(t, 7, `{"amountCents":100}`)
	assert.Equal(t, 200, w.Code)
	require.NotNil(t, mock.lastCreateReq)
	assert.Equal(t, 7, mock.lastCreateReq.NewApiUserID)
	assert.Equal(t, int64(100), mock.lastCreateReq.AmountCents)
}

// clientRequestId 缺省时由 façade 生成；浏览器重试复用同一值。
func TestWalletCreate_ClientRequestIDGeneratedWhenAbsent(t *testing.T) {
	mock := &mockBillingClient{order: &service.BillingRechargeOrder{OrderNo: "ORD", AmountCents: 5000, Currency: "CNY", Status: "PENDING"}}
	injectMockBillingClient(t, mock)

	w := callCreate(t, 42, `{"amountCents":5000}`)
	require.Equal(t, 200, w.Code)
	require.NotNil(t, mock.lastCreateReq)
	assert.NotEmpty(t, mock.lastCreateReq.ClientRequestID)
	assert.LessOrEqual(t, len(mock.lastCreateReq.ClientRequestID), 64)
}

// 错误映射：billing 内部错误码 → 用户文案 + 粗粒度 HTTP；绝不透传 code。
func TestWalletCreate_ErrorMapping_NeverLeaksCode(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{"conflict", &service.BillingRechargeError{HTTPStatus: 409, Code: service.BillingErrOrderConflict}, 409},
		{"quota", &service.BillingRechargeError{HTTPStatus: 409, Code: service.BillingErrQuotaCapacityExceeded}, 409},
		{"user_unavailable", &service.BillingRechargeError{HTTPStatus: 409, Code: service.BillingErrCreditUserUnavailable}, 403},
		{"not_found", &service.BillingRechargeError{HTTPStatus: 404, Code: service.BillingErrOrderNotFound}, 404},
		{"unauthorized_maps_to_503", &service.BillingRechargeError{HTTPStatus: 401, Code: service.BillingErrUnauthorized}, 503},
		{"transport_retryable_maps_to_503", &service.BillingRechargeError{HTTPStatus: 0, Retryable: true}, 503},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := &mockBillingClient{err: tc.err}
			injectMockBillingClient(t, mock)
			w := callCreate(t, 42, `{"amountCents":5000}`)
			assert.Equal(t, tc.wantStatus, w.Code, w.Body.String())
			// 响应体绝不包含任何 billing 稳定错误码或 "code" 字段值。
			body := w.Body.String()
			assert.NotContains(t, body, "ORDER_CONFLICT")
			assert.NotContains(t, body, "QUOTA_CAPACITY_EXCEEDED")
			assert.NotContains(t, body, "UNAUTHORIZED")
			assert.NotContains(t, body, "CREDIT_USER_UNAVAILABLE")
			// 成功包络只有 success/message，没有透传的 code 字段。
			var resp map[string]interface{}
			require.NoError(t, json.Unmarshal([]byte(body), &resp))
			_, hasCode := resp["code"]
			assert.False(t, hasCode, "browser response must not carry billing code field")
		})
	}
}

func TestWalletGet_NewApiUserIDFromContext(t *testing.T) {
	mock := &mockBillingClient{order: &service.BillingRechargeOrder{OrderNo: "ORD9", AmountCents: 5000, Currency: "CNY", Status: "PAID"}}
	injectMockBillingClient(t, mock)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("id", 42)
	c.Params = gin.Params{{Key: "orderNo", Value: "ORD9"}}
	c.Request = httptest.NewRequest("GET", "/api/wallet/recharges/ORD9", nil)
	GetWischoicerWalletRecharge(c)

	require.Equal(t, 200, w.Code, w.Body.String())
	assert.Equal(t, "ORD9", mock.lastGetOrderNo)
	assert.Equal(t, 42, mock.lastGetUser, "GET ownership filter must use context userID")
}

func TestWalletList_PassesOwnerAndLimit(t *testing.T) {
	mock := &mockBillingClient{listResult: &service.BillingListResult{
		Items: []service.BillingRechargeOrder{{OrderNo: "ORD1", AmountCents: 5000, Currency: "CNY", Status: "PAID"}},
	}}
	injectMockBillingClient(t, mock)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("id", 42)
	c.Request = httptest.NewRequest("GET", "/api/wallet/recharges?limit=5&cursor=abc", nil)
	ListWischoicerWalletRecharges(c)

	require.Equal(t, 200, w.Code, w.Body.String())
	assert.Equal(t, 42, mock.lastListUser)
	assert.Equal(t, 5, mock.lastListLimit)

	var resp struct {
		Data struct {
			Items      []map[string]interface{} `json:"items"`
			NextCursor string                   `json:"nextCursor"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Data.Items, 1)
	// 列表项同样只投影安全字段。
	assert.NotContains(t, strings.ToLower(w.Body.String()), "quota")
}
