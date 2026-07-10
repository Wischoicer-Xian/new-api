package middleware

import (
	"crypto/subtle"
	"net/http"

	"github.com/QuantumNous/new-api/common"

	"github.com/gin-gonic/gin"
)

// HeaderWischoicerBillingToken 是 wischoicer-billing 调用 new-api 内部接口时
// 携带的共享 Token header（方案 §7.4/7.5）。
const HeaderWischoicerBillingToken = "X-Internal-Service-Token"

// WischoicerBillingInternalAuth 校验 wischoicer-billing → new-api 内部接口的
// 共享 Token。所有 4 个 recharge 接口（reserve/release/credit/GET）共用此 middleware。
//
// 使用常量时间比较防止时序侧信道；Token 由启动时 fail-fast 保证非空（见
// common.initWischoicerRechargeConfig）。网络 ACL 是纵深防御，Token 是鉴权边界。
func WischoicerBillingInternalAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		provided := c.GetHeader(HeaderWischoicerBillingToken)
		expected := common.WischoicerBillingInternalServiceToken

		if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
			common.SysError("wischoicer billing internal auth failed: client_ip=" + c.ClientIP())
			c.JSON(http.StatusUnauthorized, gin.H{
				"success": false,
				"message": "UNAUTHORIZED",
			})
			c.Abort()
			return
		}
		c.Next()
	}
}
