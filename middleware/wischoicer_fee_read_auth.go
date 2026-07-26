package middleware

import (
	"net/http"

	"github.com/QuantumNous/new-api/common"

	"github.com/gin-gonic/gin"
)

// HeaderWischoicerFeeReadToken 是 user-service 调 new-api fee-read（C5）时携带的
// fee-read-token header（RFC v4 N2/U4）。与 Token B、sso-service-token 完全独立，不复用。
const HeaderWischoicerFeeReadToken = "X-Wischoicer-Fee-Read-Token"

// WischoicerFeeReadAuth 校验 user-service → new-api fee-read（C5）的入站 fee-read-token。
// C5 路由必须挂此 middleware（internal-auth group），**禁止落到 public / UserAuth**
// （RFC v4 N2 + recon D-1）。
//
// fee-read-token 双槽（current/next，WIS-547 R3 同构）：复用 wischoicerTokenMatchesAnySlot
// （SHA-256 定宽 + 两次恒时比较、不短路）。current 为空时 C5 路由根本不挂载（fail-closed，
// 见 common.initWischoicerRechargeConfig）。缺失/错误一律 401 UNAUTHORIZED，不透露
// token / 路由 / 槽位。
//
// 注：C5 落点（新增 /api/internal/wischoicer/** vs 复用 /api/log/self/feature-usage/*）
// 是 RFC v4 D3，待记星 round-4 R3 拍板；本 middleware 与落点无关，先就位。
func WischoicerFeeReadAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		provided := c.GetHeader(HeaderWischoicerFeeReadToken)
		if provided == "" {
			wischoicerFeeReadReject(c)
			return
		}
		if !wischoicerTokenMatchesAnySlot(provided,
			common.WischoicerFeeReadToken,
			common.WischoicerFeeReadTokenNext) {
			wischoicerFeeReadReject(c)
			return
		}
		c.Next()
	}
}

// wischoicerFeeReadReject 统一输出鉴权失败响应 + 401，记内部审计日志（不记 token/槽位）。
func wischoicerFeeReadReject(c *gin.Context) {
	common.SysError("wischoicer fee-read auth failed: client_ip=" + c.ClientIP())
	c.JSON(http.StatusUnauthorized, gin.H{
		"success": false,
		"code":    "UNAUTHORIZED",
		"message": "UNAUTHORIZED",
	})
	c.Abort()
}
