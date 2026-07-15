package middleware

import (
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// P2 契约（WIS-547 §0 包络）：鉴权失败必须返回 {success:false,code:"UNAUTHORIZED",message}
// 且 HTTP 401；业务方依赖 code，不得只靠 message。Token 缺失/错误统一 401，不透露细节。
func TestWischoicerBillingInternalAuth_FailureEnvelopeHasCode(t *testing.T) {
	original := common.WischoicerBillingInternalServiceToken
	common.WischoicerBillingInternalServiceToken = "expected-token"
	t.Cleanup(func() { common.WischoicerBillingInternalServiceToken = original })

	cases := []struct {
		name   string
		header string
	}{
		{"missing token", ""},
		{"wrong token", "wrong-value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			w := httptest.NewRecorder()
			c, r := gin.CreateTestContext(w)
			r.GET("/x", WischoicerBillingInternalAuth(), func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
			c.Request = httptest.NewRequest("GET", "/x", nil)
			if tc.header != "" {
				c.Request.Header.Set(HeaderWischoicerBillingToken, tc.header)
			}
			r.ServeHTTP(w, c.Request)

			assert.Equal(t, 401, w.Code)
			assert.Contains(t, w.Body.String(), `"code":"UNAUTHORIZED"`)
			assert.Contains(t, w.Body.String(), `"success":false`)
		})
	}
}

func TestWischoicerBillingInternalAuth_CorrectTokenPasses(t *testing.T) {
	original := common.WischoicerBillingInternalServiceToken
	common.WischoicerBillingInternalServiceToken = "expected-token"
	t.Cleanup(func() { common.WischoicerBillingInternalServiceToken = original })

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	_, r := gin.CreateTestContext(w)
	r.GET("/x", WischoicerBillingInternalAuth(), func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set(HeaderWischoicerBillingToken, "expected-token")
	r.ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)
}

// P1-2（WIS-547 R3 已锁 24h 双槽轮换）：接收端对 current/next 两个同方向 token 都
// constant-time 校验、不短路；轮换窗口内两者都接受，撤销后旧 token 失效。
func setTokenB(t *testing.T, current, next string) {
	t.Helper()
	common.WischoicerBillingInternalServiceToken = current
	common.WischoicerBillingInternalServiceTokenNext = next
}

func TestWischoicerBillingInternalAuth_DualSlotCurrentAndNext(t *testing.T) {
	origCur := common.WischoicerBillingInternalServiceToken
	origNext := common.WischoicerBillingInternalServiceTokenNext
	t.Cleanup(func() {
		common.WischoicerBillingInternalServiceToken = origCur
		common.WischoicerBillingInternalServiceTokenNext = origNext
	})

	cases := []struct {
		name     string
		cur      string
		next     string
		provided string
		want     int
	}{
		{"current matches", "cur", "next", "cur", 200},
		{"next matches (rotation window)", "cur", "next", "next", 200},
		{"wrong value rejected", "cur", "next", "nope", 401},
		{"revoked token (neither) rejected", "newcur", "newnext", "cur", 401},
		{"only current set, current matches", "cur", "", "cur", 200},
		{"only current set, wrong rejected", "cur", "", "other", 401},
		{"missing header rejected", "cur", "next", "", 401},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setTokenB(t, tc.cur, tc.next)
			gin.SetMode(gin.TestMode)
			w := httptest.NewRecorder()
			_, r := gin.CreateTestContext(w)
			r.GET("/x", WischoicerBillingInternalAuth(), func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
			req := httptest.NewRequest("GET", "/x", nil)
			if tc.provided != "" {
				req.Header.Set(HeaderWischoicerBillingToken, tc.provided)
			}
			r.ServeHTTP(w, req)
			assert.Equal(t, tc.want, w.Code)
		})
	}
}

// 轮换切换 → 撤销：old 仍在 next 时被接受；next 清空后 old 失效。
func TestWischoicerBillingInternalAuth_RotationSwitchThenRevoke(t *testing.T) {
	origCur := common.WischoicerBillingInternalServiceToken
	origNext := common.WischoicerBillingInternalServiceTokenNext
	t.Cleanup(func() {
		common.WischoicerBillingInternalServiceToken = origCur
		common.WischoicerBillingInternalServiceTokenNext = origNext
	})

	doReq := func(provided string) int {
		gin.SetMode(gin.TestMode)
		w := httptest.NewRecorder()
		_, r := gin.CreateTestContext(w)
		r.GET("/x", WischoicerBillingInternalAuth(), func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
		req := httptest.NewRequest("GET", "/x", nil)
		if provided != "" {
			req.Header.Set(HeaderWischoicerBillingToken, provided)
		}
		r.ServeHTTP(w, req)
		return w.Code
	}

	// 1) 只有 old token 在用。
	setTokenB(t, "old", "")
	assert.Equal(t, 200, doReq("old"))

	// 2) 轮换窗口：current=new、next=old → 两者都接受（双槽并行）。
	setTokenB(t, "new", "old")
	assert.Equal(t, 200, doReq("new"))
	assert.Equal(t, 200, doReq("old"), "old must still be accepted via next during rotation window")

	// 3) 撤销完成：next 清空 → old 失效，new 仍接受。
	setTokenB(t, "new", "")
	assert.Equal(t, 200, doReq("new"))
	assert.Equal(t, 401, doReq("old"), "revoked old token must be rejected after next cleared")
}
