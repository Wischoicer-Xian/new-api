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
// Token B 双槽（WIS-547 R3 已锁 24h current/next 无损轮换）：对 current 与 next 两个
// 同方向 token 都做 constant-time 比较，且**不短路**——两个比较都执行后再 OR，避免通过
// 时序或分支泄露命中的是哪一槽。next 为空时比较结果恒为 0（provided 非空、长度不同），
// 不会误接受；current 为空时路由根本不挂载（fail-closed，见 common.initWischoicerRechargeConfig）。
// 缺失/错误一律 401 UNAUTHORIZED，不透露 token、路由或槽位。网络 ACL 是纵深防御，Token 是鉴权边界。
func WischoicerBillingInternalAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		provided := c.GetHeader(HeaderWischoicerBillingToken)
		if provided == "" {
			wischoicerBillingReject(c)
			return
		}
		current := common.WischoicerBillingInternalServiceToken
		next := common.WischoicerBillingInternalServiceTokenNext
		// 两次 constant-time 比较都执行（不短路），仅按 OR 结果放行。
		curMatch := subtle.ConstantTimeCompare([]byte(provided), []byte(current))
		nextMatch := subtle.ConstantTimeCompare([]byte(provided), []byte(next))
		if curMatch != 1 && nextMatch != 1 {
			wischoicerBillingReject(c)
			return
		}
		c.Next()
	}
}

// wischoicerBillingReject 统一输出鉴权失败响应：{success:false,code:"UNAUTHORIZED",message}
// （WIS-547 §0 锁定包络）+ 401，并记内部审计日志（绝不记 token / 槽位）。
func wischoicerBillingReject(c *gin.Context) {
	common.SysError("wischoicer billing internal auth failed: client_ip=" + c.ClientIP())
	c.JSON(http.StatusUnauthorized, gin.H{
		"success": false,
		"code":    "UNAUTHORIZED",
		"message": "UNAUTHORIZED",
	})
	c.Abort()
}
