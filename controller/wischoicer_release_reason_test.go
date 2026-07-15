package controller

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// P2（WIS-547 §2 受控枚举）：release reason 必须落在 billing actual enum（"closed"）
// ∪ 契约显式枚举 {closed, expired, prepay_failed}；禁止任意字符串。
func TestIsWischoicerValidReleaseReason_Enum(t *testing.T) {
	for _, r := range []string{"closed", "expired", "prepay_failed"} {
		assert.True(t, isWischoicerValidReleaseReason(r), "%q should be valid", r)
	}
	for _, r := range []string{"", "garbage", "CLOSED", "closed ", " refund", "refund", "cancelled"} {
		assert.False(t, isWischoicerValidReleaseReason(r), "%q should be invalid", r)
	}
}

// 非 enum reason 在进入 model（DB）之前即被 400 拒绝——DB-free 验证。
func TestPostWischoicerRechargeReservationRelease_RejectsBadReason(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"garbage reason", `{"reason":"garbage"}`},
		{"empty reason", `{"reason":""}`},
		{"not in enum", `{"reason":"refund"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Params = gin.Params{{Key: "orderNo", Value: "ORD1"}}
			c.Request = httptest.NewRequest("POST", "/", strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			PostWischoicerRechargeReservationRelease(c)
			assert.Equal(t, 400, w.Code, "bad reason must be rejected before model/DB")
			assert.Contains(t, w.Body.String(), "INVALID_ARGUMENT")
		})
	}
}
