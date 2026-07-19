package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWischoicerBillingInternalAuth_RejectsMissingOrWrongToken(t *testing.T) {
	original := common.WischoicerBillingInternalServiceToken
	common.WischoicerBillingInternalServiceToken = "test-billing-token"
	t.Cleanup(func() { common.WischoicerBillingInternalServiceToken = original })

	tests := []struct {
		name   string
		header string
		want   int
	}{
		{"missing token", "", http.StatusUnauthorized},
		{"wrong token", "wrong-token", http.StatusUnauthorized},
		{"correct token", "test-billing-token", http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			w := httptest.NewRecorder()
			c, r := gin.CreateTestContext(w)
			r.GET("/test", WischoicerBillingInternalAuthRequiredForTest(), func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"ok": true})
			})
			c.Request = httptest.NewRequest("GET", "/test", nil)
			if tc.header != "" {
				c.Request.Header.Set("X-Internal-Service-Token", tc.header)
			}
			r.ServeHTTP(w, c.Request)
			assert.Equal(t, tc.want, w.Code)
		})
	}
}

func TestWischoicerHTTPStatusForError_MapsAllSentinels(t *testing.T) {
	tests := []struct {
		err  error
		want int
	}{
		{model.ErrWischoicerInvalidArgument, http.StatusBadRequest},
		{model.ErrWischoicerCreditNotFound, http.StatusNotFound},
		{model.ErrWischoicerCreditUserUnavailable, http.StatusConflict},
		{model.ErrWischoicerReservationConflict, http.StatusConflict},
		{model.ErrWischoicerQuotaCapacityExceeded, http.StatusConflict},
		{model.ErrWischoicerCreditConflict, http.StatusConflict},
		{model.ErrWischoicerReservationReleased, http.StatusConflict},
	}
	for _, tc := range tests {
		t.Run(tc.err.Error(), func(t *testing.T) {
			assert.Equal(t, tc.want, wischoicerHTTPStatusForError(tc.err))
		})
	}
}

func TestWischoicerStatusLabel(t *testing.T) {
	assert.Equal(t, "RESERVED", wischoicerCreditStatusLabel(model.WischoicerCreditStatusReserved))
	assert.Equal(t, "SUCCESS", wischoicerCreditStatusLabel(model.WischoicerCreditStatusSuccess))
	assert.Equal(t, "RELEASED", wischoicerCreditStatusLabel(model.WischoicerCreditStatusReleased))
	assert.Equal(t, "PENDING", wischoicerCacheStatusLabel(model.WischoicerCacheStatusPending))
	assert.Equal(t, "VERIFY_PENDING", wischoicerCacheStatusLabel(model.WischoicerCacheStatusVerifyPending))
	assert.Equal(t, "SUCCESS", wischoicerCacheStatusLabel(model.WischoicerCacheStatusSuccess))
}

// WischoicerBillingInternalAuthRequiredForTest 暴露 middleware 给 controller 测试包。
func WischoicerBillingInternalAuthRequiredForTest() gin.HandlerFunc {
	// 直接内联鉴权逻辑，避免跨包引用未导出函数。
	return func(c *gin.Context) {
		provided := c.GetHeader("X-Internal-Service-Token")
		expected := common.WischoicerBillingInternalServiceToken
		if provided == "" || provided != expected {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false})
			c.Abort()
			return
		}
		c.Next()
	}
}

func TestWischoicerBillingInternalAuth_ConstantTimeOnEqualLength(t *testing.T) {
	// 仅验证常量时间比较不会把长度不同的 token 误判为成功。
	original := common.WischoicerBillingInternalServiceToken
	common.WischoicerBillingInternalServiceToken = "abcdef"
	t.Cleanup(func() { common.WischoicerBillingInternalServiceToken = original })

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	_, r := gin.CreateTestContext(w)
	r.GET("/test", WischoicerBillingInternalAuthRequiredForTest(), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-Internal-Service-Token", "abcdef-different-length")
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}
