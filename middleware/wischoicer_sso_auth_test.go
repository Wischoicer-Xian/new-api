package middleware

import (
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// RFC v4 N2（WIS-547 §0 包络同构）：C2 sso-service-token / C5 fee-read-token 鉴权失败
// 必须返回 {success:false,code:"UNAUTHORIZED",message} + HTTP 401；token 缺失/错误统一 401，
// 不透露 token/路由/槽位。current/next 双槽恒时校验，任一命中即放行。

func TestWischoicerSsoInternalAuth_FailureEnvelopeHasCode(t *testing.T) {
	origCur, origNext := common.WischoicerSsoServiceToken, common.WischoicerSsoServiceTokenNext
	common.WischoicerSsoServiceToken = "expected-sso-token"
	common.WischoicerSsoServiceTokenNext = ""
	t.Cleanup(func() {
		common.WischoicerSsoServiceToken, common.WischoicerSsoServiceTokenNext = origCur, origNext
	})

	for _, tc := range []struct{ name, header string }{
		{"missing token", ""},
		{"wrong token", "wrong-value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			w := httptest.NewRecorder()
			_, r := gin.CreateTestContext(w)
			r.POST("/x", WischoicerSsoInternalAuth(), func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
			req := httptest.NewRequest("POST", "/x", nil)
			if tc.header != "" {
				req.Header.Set(HeaderWischoicerSsoToken, tc.header)
			}
			r.ServeHTTP(w, req)
			assert.Equal(t, 401, w.Code)
			assert.Contains(t, w.Body.String(), `"code":"UNAUTHORIZED"`)
			assert.Contains(t, w.Body.String(), `"success":false`)
		})
	}
}

func TestWischoicerSsoInternalAuth_CurrentOrNextSlotPasses(t *testing.T) {
	origCur, origNext := common.WischoicerSsoServiceToken, common.WischoicerSsoServiceTokenNext
	common.WischoicerSsoServiceToken = "current-sso"
	common.WischoicerSsoServiceTokenNext = "next-sso"
	t.Cleanup(func() {
		common.WischoicerSsoServiceToken, common.WischoicerSsoServiceTokenNext = origCur, origNext
	})

	for _, tc := range []struct{ name, header string }{
		{"current slot", "current-sso"},
		{"next slot (rotation window)", "next-sso"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			w := httptest.NewRecorder()
			_, r := gin.CreateTestContext(w)
			r.POST("/x", WischoicerSsoInternalAuth(), func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
			req := httptest.NewRequest("POST", "/x", nil)
			req.Header.Set(HeaderWischoicerSsoToken, tc.header)
			r.ServeHTTP(w, req)
			assert.Equal(t, 200, w.Code)
		})
	}
}

func TestWischoicerFeeReadAuth_FailureEnvelopeHasCode(t *testing.T) {
	origCur, origNext := common.WischoicerFeeReadToken, common.WischoicerFeeReadTokenNext
	common.WischoicerFeeReadToken = "expected-fee-token"
	common.WischoicerFeeReadTokenNext = ""
	t.Cleanup(func() {
		common.WischoicerFeeReadToken, common.WischoicerFeeReadTokenNext = origCur, origNext
	})

	for _, tc := range []struct{ name, header string }{
		{"missing token", ""},
		{"wrong token", "wrong-value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			w := httptest.NewRecorder()
			_, r := gin.CreateTestContext(w)
			r.GET("/x", WischoicerFeeReadAuth(), func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
			req := httptest.NewRequest("GET", "/x", nil)
			if tc.header != "" {
				req.Header.Set(HeaderWischoicerFeeReadToken, tc.header)
			}
			r.ServeHTTP(w, req)
			assert.Equal(t, 401, w.Code)
			assert.Contains(t, w.Body.String(), `"code":"UNAUTHORIZED"`)
			assert.Contains(t, w.Body.String(), `"success":false`)
		})
	}
}

func TestWischoicerFeeReadAuth_CurrentOrNextSlotPasses(t *testing.T) {
	origCur, origNext := common.WischoicerFeeReadToken, common.WischoicerFeeReadTokenNext
	common.WischoicerFeeReadToken = "current-fee"
	common.WischoicerFeeReadTokenNext = "next-fee"
	t.Cleanup(func() {
		common.WischoicerFeeReadToken, common.WischoicerFeeReadTokenNext = origCur, origNext
	})

	for _, tc := range []struct{ name, header string }{
		{"current slot", "current-fee"},
		{"next slot (rotation window)", "next-fee"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			w := httptest.NewRecorder()
			_, r := gin.CreateTestContext(w)
			r.GET("/x", WischoicerFeeReadAuth(), func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
			req := httptest.NewRequest("GET", "/x", nil)
			req.Header.Set(HeaderWischoicerFeeReadToken, tc.header)
			r.ServeHTTP(w, req)
			assert.Equal(t, 200, w.Code)
		})
	}
}

// 记星 R2 P1 (f726d64e): wischoicerTokenMatchesAnySlot 用位聚合 (matchCur|matchNext)==1，
// 非短路 ||（objdump 可见 || 的 matchCur 条件跳转）。钉住双槽聚合正确性——helper 服务
// billing/SSO/fee-read 三条 middleware，本测试覆盖其纯比较语义。
func TestWischoicerTokenMatchesAnySlot_BitOrAggregation(t *testing.T) {
	cases := []struct {
		name                    string
		provided, current, next string
		want                    bool
	}{
		{"both wrong", "p", "c", "n", false},
		{"current matches", "c", "c", "n", true},
		{"next matches", "n", "c", "n", true},
		{"both match same value", "x", "x", "x", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := wischoicerTokenMatchesAnySlot(tc.provided, tc.current, tc.next)
			if got != tc.want {
				t.Fatalf("matchesAnySlot(provided=%q,current=%q,next=%q) = %v, want %v",
					tc.provided, tc.current, tc.next, got, tc.want)
			}
		})
	}
}
