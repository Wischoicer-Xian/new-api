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
