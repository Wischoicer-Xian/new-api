package middleware

import (
	"net/http"

	"github.com/QuantumNous/new-api/common"

	"github.com/gin-gonic/gin"
)

// HeaderWischoicerSsoToken 是 user-service 调 new-api SSO authorize（C2）时携带的
// sso-service-token header（RFC v4 N2）。与 Token B（HeaderWischoicerBillingToken）、
// fee-read-token（HeaderWischoicerFeeReadToken）完全独立，绝不复用。
const HeaderWischoicerSsoToken = "X-Wischoicer-Sso-Service-Token"

// WischoicerSsoInternalAuth 校验 user-service → new-api SSO authorize（C2，
// POST /api/sso/wischoicer/authorize）的入站 sso-service-token。C2 路由必须挂此
// middleware（internal-auth group），**禁止落到 public apiRouter / UserAuth**（RFC v4 N2
// + recon D-1：公网 edge 保留 public/UserAuth UI surface，仅显式拒 C2/C5）。
//
// sso-service-token 双槽（current/next，WIS-547 R3 同构）：provided 命中 current 或 next
// 任一即放行。复用 wischoicerTokenMatchesAnySlot（SHA-256 定宽 + 两次恒时比较、不短路），
// 与 Token B 接收端同构——长度差被抹平、无提前返回、不泄露命中的是哪一槽。current 为空时
// C2 路由根本不挂载（fail-closed，见 common.initWischoicerRechargeConfig）。缺失/错误一律
// 401 UNAUTHORIZED，不透露 token / 路由 / 槽位。网络 ACL 是纵深防御，Token 是鉴权边界。
func WischoicerSsoInternalAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		provided := c.GetHeader(HeaderWischoicerSsoToken)
		if provided == "" {
			wischoicerSsoReject(c)
			return
		}
		if !wischoicerTokenMatchesAnySlot(provided,
			common.WischoicerSsoServiceToken,
			common.WischoicerSsoServiceTokenNext) {
			wischoicerSsoReject(c)
			return
		}
		c.Next()
	}
}

// wischoicerSsoReject 统一输出鉴权失败响应：{success:false,code:"UNAUTHORIZED",message}
// + 401，并记内部审计日志（绝不记 token / 槽位）。
func wischoicerSsoReject(c *gin.Context) {
	common.SysError("wischoicer sso internal auth failed: client_ip=" + c.ClientIP())
	c.JSON(http.StatusUnauthorized, gin.H{
		"success": false,
		"code":    "UNAUTHORIZED",
		"message": "UNAUTHORIZED",
	})
	c.Abort()
}
