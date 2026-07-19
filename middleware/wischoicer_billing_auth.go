package middleware

import (
	"crypto/sha256"
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
// Token B 双槽（WIS-547 R3 已锁 24h current/next 无损轮换）：provided 命中 current 或
// next 任一即放行。为消除「subtle.ConstantTimeCompare 在长度不同时提前返回」造成的时序
// 差（R2 复审 P2），先把三者 SHA-256 到 32B 定宽，再做两次恒时比较、不短路——current/next
// 长度差被抹平，无法据响应时序区分命中的是哪一槽。next 为空时其摘要恒定，provided 非空
// 永不命中；current 为空时路由根本不挂载（fail-closed，见 common.initWischoicerRechargeConfig）。
// 缺失/错误一律 401 UNAUTHORIZED，不透露 token、路由或槽位。网络 ACL 是纵深防御，Token 是鉴权边界。
func WischoicerBillingInternalAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		provided := c.GetHeader(HeaderWischoicerBillingToken)
		if provided == "" {
			wischoicerBillingReject(c)
			return
		}
		if !wischoicerTokenMatchesAnySlot(provided,
			common.WischoicerBillingInternalServiceToken,
			common.WischoicerBillingInternalServiceTokenNext) {
			wischoicerBillingReject(c)
			return
		}
		c.Next()
	}
}

// wischoicerTokenMatchesAnySlot 判定 provided 是否命中 current 或 next 任一槽，恒时。
//
// subtle.ConstantTimeCompare(x, y) 在 len(x) != len(y) 时立即返回 0（非恒时）。若直接拿
// provided 与 current/next 原文比较，两槽长度差 + 两次比较的提前返回会让响应时序泄露
// 「provided 长度像哪一槽」。先把 provided / current / next 都 SHA-256 到 32B 定宽，再做
// 两次恒时比较并 OR：长度差被抹平，提前返回不复存在，两槽都执行不短路。
func wischoicerTokenMatchesAnySlot(provided, current, next string) bool {
	p := sha256.Sum256([]byte(provided))
	cur := sha256.Sum256([]byte(current))
	nxt := sha256.Sum256([]byte(next))
	matchCur := subtle.ConstantTimeCompare(p[:], cur[:])
	matchNext := subtle.ConstantTimeCompare(p[:], nxt[:])
	return matchCur == 1 || matchNext == 1
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
