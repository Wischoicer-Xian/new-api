package controller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockBillingClient 记录 façade 传入的归属/金额，返回预设订单或错误。
type mockBillingClient struct {
	createCalled    bool
	createReq       service.BillingCreateOrderRequest
	createOrder     *service.BillingOrder
	createErr       error
	getCalled       bool
	getOrderNo      string
	getNewApiUserID int
	getOrder        *service.BillingOrder
	getErr          error
}

func (m *mockBillingClient) CreateOrder(_ context.Context, req service.BillingCreateOrderRequest) (*service.BillingOrder, error) {
	m.createCalled = true
	m.createReq = req
	if m.createErr != nil {
		return nil, m.createErr
	}
	return m.createOrder, nil
}

func (m *mockBillingClient) GetOrder(_ context.Context, orderNo string, newApiUserId int) (*service.BillingOrder, error) {
	m.getCalled = true
	m.getOrderNo = orderNo
	m.getNewApiUserID = newApiUserId
	if m.getErr != nil {
		return nil, m.getErr
	}
	return m.getOrder, nil
}

func i64p(v int64) *int64 { return &v }

// newWalletTestEngine 构造一个挂载 façade 路由、并模拟 UserAuth（c.Set("id", userID)）的 gin 引擎。
func newWalletTestEngine(t *testing.T, userID int) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("id", userID); c.Next() })
	r.GET("/api/wallet/recharges/options", WischoicerWalletRechargeOptions)
	r.POST("/api/wallet/recharges", CreateWischoicerWalletRecharge)
	r.GET("/api/wallet/recharges/:orderNo", GetWischoicerWalletRecharge)
	return r
}

type walletAPIResp struct {
	Success bool            `json:"success"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func doWalletRequest(t *testing.T, r *gin.Engine, method, path, body string) (*walletAPIResp, *httptest.ResponseRecorder) {
	t.Helper()
	w := httptest.NewRecorder()
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	r.ServeHTTP(w, req)
	resp := &walletAPIResp{}
	if w.Body.Len() > 0 {
		require.NoError(t, common.Unmarshal(w.Body.Bytes(), resp))
	}
	return resp, w
}

func TestWischoicerWallet_Options_ReturnsFourTiers(t *testing.T) {
	restore := setWischoicerWalletClientForTest(&mockBillingClient{})
	t.Cleanup(restore)

	r := newWalletTestEngine(t, 42)
	resp, w := doWalletRequest(t, r, "GET", "/api/wallet/recharges/options", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.True(t, resp.Success)

	var opts walletRechargeOptionsResponse
	require.NoError(t, common.Unmarshal(resp.Data, &opts))
	assert.Equal(t, "CNY", opts.Currency)
	assert.Equal(t, []int64{5000, 10000, 20000, 50000}, []int64{
		opts.Tiers[0].AmountCents, opts.Tiers[1].AmountCents, opts.Tiers[2].AmountCents, opts.Tiers[3].AmountCents,
	})
	assert.Equal(t, int64(5000), opts.MinCents)
	assert.Equal(t, int64(50000), opts.MaxCents)
	// ¥1（amountCents=100）绝不作为公开档返回：没有任何 tier 命中 100。
	for _, tier := range opts.Tiers {
		assert.NotEqual(t, int64(100), tier.AmountCents, "¥1 test tier must not be publicly listed")
	}
}

func TestWischoicerWallet_Create_HappyPath_ProjectsSafeFields(t *testing.T) {
	mc := &mockBillingClient{createOrder: &service.BillingOrder{
		OrderNo: "O1", AmountCents: 5000, Currency: "CNY", Status: "PENDING", CodeUrl: "wx://qr", ExpireTime: i64p(1700000000),
	}}
	restore := setWischoicerWalletClientForTest(mc)
	t.Cleanup(restore)

	r := newWalletTestEngine(t, 42)
	resp, w := doWalletRequest(t, r, "POST", "/api/wallet/recharges",
		`{"amountCents":5000,"clientRequestId":"abc"}`)
	require.True(t, resp.Success, "body="+w.Body.String())

	var view walletOrderView
	require.NoError(t, common.Unmarshal(resp.Data, &view))
	assert.Equal(t, "O1", view.OrderNo)
	assert.Equal(t, int64(5000), view.AmountCents)
	assert.Equal(t, "PENDING", view.Status)
	assert.Equal(t, "wx://qr", view.CodeUrl)
	require.NotNil(t, view.ExpireTime)
	assert.Equal(t, int64(1700000000), *view.ExpireTime)

	// 归属只来自 context（42），与 body 无关。
	assert.True(t, mc.createCalled)
	assert.Equal(t, 42, mc.createReq.NewApiUserId)
	// 安全面：响应不含 quota / 内部归属字段。
	bodyStr := w.Body.String()
	assert.NotContains(t, bodyStr, "quota")
	assert.NotContains(t, bodyStr, "newApiUserId")
	assert.NotContains(t, bodyStr, "new_api_user_id")
}

func TestWischoicerWallet_Create_IgnoresBodyUserId(t *testing.T) {
	mc := &mockBillingClient{createOrder: &service.BillingOrder{OrderNo: "O1", AmountCents: 5000, Currency: "CNY", Status: "PENDING"}}
	restore := setWischoicerWalletClientForTest(mc)
	t.Cleanup(restore)

	r := newWalletTestEngine(t, 42)
	// 浏览器试图伪造归属：body 里塞 newApiUserId/userId/tokenA，必须被忽略。
	resp, _ := doWalletRequest(t, r, "POST", "/api/wallet/recharges",
		`{"amountCents":5000,"clientRequestId":"abc","newApiUserId":999,"userId":888,"tokenA":"stolen"}`)
	require.True(t, resp.Success)
	assert.Equal(t, 42, mc.createReq.NewApiUserId, "user id must come from UserAuth context, never body")
}

func TestWischoicerWallet_Create_RejectsInvalidAmount(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		called bool
		ok     bool
	}{
		{"non-tier amount 30000", `{"amountCents":30000,"clientRequestId":"abc"}`, false, false},
		{"one yuan by normal user", `{"amountCents":100,"clientRequestId":"abc"}`, false, false},
		{"zero amount", `{"amountCents":0,"clientRequestId":"abc"}`, false, false},
		{"valid tier 5000", `{"amountCents":5000,"clientRequestId":"abc"}`, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mc := &mockBillingClient{createOrder: &service.BillingOrder{OrderNo: "O1", AmountCents: 5000, Currency: "CNY", Status: "PENDING"}}
			restore := setWischoicerWalletClientForTest(mc)
			t.Cleanup(restore)

			r := newWalletTestEngine(t, 42)
			resp, _ := doWalletRequest(t, r, "POST", "/api/wallet/recharges", tc.body)
			assert.Equal(t, tc.called, mc.createCalled)
			assert.Equal(t, tc.ok, resp.Success)
		})
	}
}

func TestWischoicerWallet_Create_AllowsOneYuanForWhitelistedTestUser(t *testing.T) {
	// 把 42 加入 ¥1 测试白名单，测试后还原。
	orig := common.WischoicerRechargeTestUserIDs
	common.WischoicerRechargeTestUserIDs = map[int]struct{}{42: {}}
	t.Cleanup(func() { common.WischoicerRechargeTestUserIDs = orig })

	// 白名单内账号 42：¥1 可达。
	mc := &mockBillingClient{createOrder: &service.BillingOrder{OrderNo: "T1", AmountCents: 100, Currency: "CNY", Status: "PENDING"}}
	restore := setWischoicerWalletClientForTest(mc)
	t.Cleanup(restore)

	r := newWalletTestEngine(t, 42)
	resp, _ := doWalletRequest(t, r, "POST", "/api/wallet/recharges", `{"amountCents":100,"clientRequestId":"t"}`)
	require.True(t, resp.Success)
	assert.Equal(t, int64(100), mc.createReq.AmountCents)

	// 非白名单账号 43：¥1 仍被拒。
	mc2 := &mockBillingClient{createOrder: &service.BillingOrder{OrderNo: "T2", AmountCents: 100, Currency: "CNY", Status: "PENDING"}}
	restore2 := setWischoicerWalletClientForTest(mc2)
	t.Cleanup(restore2)
	r2 := newWalletTestEngine(t, 43)
	resp2, _ := doWalletRequest(t, r2, "POST", "/api/wallet/recharges", `{"amountCents":100,"clientRequestId":"t"}`)
	assert.False(t, resp2.Success)
	assert.False(t, mc2.createCalled)
}

func TestWischoicerWallet_Create_RejectsBadClientRequestId(t *testing.T) {
	cases := []struct {
		name   string
		rid    string
		called bool
		ok     bool
	}{
		{"empty", "", false, false},
		{"too long (65)", strings.Repeat("a", 65), false, false},
		{"max length (64)", strings.Repeat("a", 64), true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mc := &mockBillingClient{createOrder: &service.BillingOrder{OrderNo: "O1", AmountCents: 5000, Currency: "CNY", Status: "PENDING"}}
			restore := setWischoicerWalletClientForTest(mc)
			t.Cleanup(restore)
			r := newWalletTestEngine(t, 42)
			body := `{"amountCents":5000,"clientRequestId":"` + tc.rid + `"}`
			resp, _ := doWalletRequest(t, r, "POST", "/api/wallet/recharges", body)
			assert.Equal(t, tc.called, mc.createCalled)
			assert.Equal(t, tc.ok, resp.Success)
		})
	}
}

func TestWischoicerWallet_Create_MapsBillingErrorsSafely(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		forbid string // 响应里绝不可出现的内部串
	}{
		{"order conflict", service.ErrBillingOrderConflict, "ORDER_CONFLICT"},
		{"capacity exceeded", service.ErrBillingCapacityExceeded, "QUOTA_CAPACITY_EXCEEDED"},
		{"user unavailable", service.ErrBillingUserUnavailable, "CREDIT_USER_UNAVAILABLE"},
		{"unavailable retryable", service.ErrBillingUnavailable, "billing"},
		{"unauthorized", service.ErrBillingUnauthorized, "UNAUTHORIZED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mc := &mockBillingClient{createErr: tc.err}
			restore := setWischoicerWalletClientForTest(mc)
			t.Cleanup(restore)
			r := newWalletTestEngine(t, 42)
			resp, w := doWalletRequest(t, r, "POST", "/api/wallet/recharges",
				`{"amountCents":5000,"clientRequestId":"abc"}`)
			assert.False(t, resp.Success)
			assert.NotEmpty(t, resp.Message)
			assert.NotContains(t, w.Body.String(), tc.forbid)
			assert.NotContains(t, w.Body.String(), "quota")
		})
	}
}

func TestWischoicerWallet_Get_HappyAndNotFound(t *testing.T) {
	mc := &mockBillingClient{getOrder: &service.BillingOrder{
		OrderNo: "O1", AmountCents: 10000, Currency: "CNY", Status: "PAID", PaidTime: i64p(1700000000),
	}}
	restore := setWischoicerWalletClientForTest(mc)
	t.Cleanup(restore)

	r := newWalletTestEngine(t, 42)
	resp, w := doWalletRequest(t, r, "GET", "/api/wallet/recharges/O1", "")
	require.True(t, resp.Success)
	// 归属来自 context，且 orderNo 从路径解析。
	assert.Equal(t, 42, mc.getNewApiUserID)
	assert.Equal(t, "O1", mc.getOrderNo)

	var view walletOrderView
	require.NoError(t, common.Unmarshal(resp.Data, &view))
	assert.Equal(t, "O1", view.OrderNo)
	assert.Equal(t, "PAID", view.Status)
	require.NotNil(t, view.PaidTime)
	assert.NotContains(t, w.Body.String(), "quota")

	// 订单不存在：安全文案，不泄露内部 code。
	mc2 := &mockBillingClient{getErr: service.ErrBillingOrderNotFound}
	restore2 := setWischoicerWalletClientForTest(mc2)
	t.Cleanup(restore2)
	r2 := newWalletTestEngine(t, 42)
	resp2, w2 := doWalletRequest(t, r2, "GET", "/api/wallet/recharges/missing", "")
	assert.False(t, resp2.Success)
	assert.NotContains(t, w2.Body.String(), "ORDER_NOT_FOUND")
}

func TestWischoicerWallet_StatusWhitelist(t *testing.T) {
	assert.Equal(t, "PAID", wischoicerWalletSafeStatus("PAID"))
	assert.Equal(t, "PENDING", wischoicerWalletSafeStatus("PENDING"))
	assert.Equal(t, "EXPIRED", wischoicerWalletSafeStatus("EXPIRED"))
	// 内部/未知状态绝不透传。
	assert.Equal(t, "UNKNOWN", wischoicerWalletSafeStatus("RESERVED_CREDIT_INTERNAL"))
	assert.Equal(t, "UNKNOWN", wischoicerWalletSafeStatus(""))
}

func TestWischoicerWallet_NoLoginDefends(t *testing.T) {
	mc := &mockBillingClient{createOrder: &service.BillingOrder{OrderNo: "O1", AmountCents: 5000, Currency: "CNY", Status: "PENDING"}}
	restore := setWischoicerWalletClientForTest(mc)
	t.Cleanup(restore)

	// userID=0：handler 防御性拒绝，绝不调 billing。
	r := newWalletTestEngine(t, 0)
	resp, _ := doWalletRequest(t, r, "POST", "/api/wallet/recharges", `{"amountCents":5000,"clientRequestId":"abc"}`)
	assert.False(t, resp.Success)
	assert.False(t, mc.createCalled)
}
