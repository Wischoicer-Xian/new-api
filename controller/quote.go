package controller

import (
	"encoding/json"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
)

// ─── WIS-460 preflight：发车前额度预检（new-api 层，internal/admin only）───
//
// POST /api/preflight-quote（AdminAuth）：供 wischoicer-user 门面（X-Internal-Service-Token → admin token）
// 在 content-workstation Step2 发车前调用，问「该用户按现行计费规则能否启动一次 image2 生成」。
//
// 纯读（service.ComputePreflightQuote）：复用 ModelPriceHelper 算 required_quota + 只读 funding 判定，
// 绝不 reserve/decrease（记星命门）。本接口不替代运行时 last_error/watchdog/前端显错（记星 P1#5）。

type preflightQuoteRequest struct {
	UserId int `json:"user_id" binding:"required"`
	// Body 是真实 image2 请求体镜像（/v1/images/generations 或 /edits 的 body）；非空时优先解析它
	// 得 prompt/size/quality，并原样喂 tiered_expr（记星 R2 P1#2：不可截断真实 billing 输入）。
	Body json.RawMessage `json:"body"`
	// Headers 是真实 header 镜像；tiered_expr 的 header()/param() 可能读。
	Headers map[string]string `json:"headers"`
	// 便捷回退：无 body 时用这几个字段构 ImageRequest（model+prompt+size+quality）。
	Model   string `json:"model"`
	Prompt  string `json:"prompt"`
	Size    string `json:"size"`
	Quality string `json:"quality"`
}

// QuotePreConsume 是预检入口（AdminAuth）。技术失败（reason=internal_error）回 5xx 供门面 fail-open
// 判定；其余（ok / insufficient / model_price_not_configured）回 200 + 契约（阻断决策在 content-workstation）。
func QuotePreConsume(c *gin.Context) {
	var req preflightQuoteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "invalid request: "+err.Error())
		return
	}

	result := service.ComputePreflightQuote(c, req.UserId, service.QuoteInput{
		Model:   req.Model,
		Prompt:  req.Prompt,
		Size:    req.Size,
		Quality: req.Quality,
		Body:    req.Body,
		Headers: req.Headers,
	})

	if result.Reason == "internal_error" {
		c.JSON(http.StatusInternalServerError, gin.H{"data": result})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": result})
}
