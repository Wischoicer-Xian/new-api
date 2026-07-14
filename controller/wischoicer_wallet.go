package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
)

// ---------------------------------------------------------------------------
// Wischoicer 钱包充值 façade（浏览器 ↔ new-api，UserAuth；WIS-547 §1 / WIS-550）
//
// 浏览器只与本 façade 交互。numeric user id 只从 UserAuth context（c.GetInt("id")）派生，
// 绝不从 body/header 接收——即便客户端在 body 里塞 newApiUserId/userId 也会被忽略。本 façade
// 以服务身份（Token A）调 billing 内部订单接口，并只把面向用户的安全字段（人民币金额、订单号、
// 状态、二维码、过期/支付时间）投影给浏览器。quota / 内部错误码 / 服务名 / token 一律不返回。
// ---------------------------------------------------------------------------

// wischoicerWalletSafeStatus 把 billing 订单状态投影到面向用户的安全白名单。
// 白名单外的串统一为 UNKNOWN，绝不把内部状态透传给浏览器。
// （状态词汇需与 S3 billing 订单状态最终对齐；当前以防御性白名单为准。）
func wischoicerWalletSafeStatus(s string) string {
	switch s {
	case "PENDING", "PENDING_PAYMENT", "AWAITING_PAYMENT", "PAID", "SUCCESS",
		"CLOSED", "EXPIRED", "REFUNDED", "FAILED":
		return s
	default:
		return "UNKNOWN"
	}
}

type walletRechargeTier struct {
	AmountCents int64  `json:"amountCents"`
	Display     string `json:"display"`
}

type walletRechargeOptionsResponse struct {
	Currency string               `json:"currency"`
	Tiers    []walletRechargeTier `json:"tiers"`
	MinCents int64                `json:"minCents"`
	MaxCents int64                `json:"maxCents"`
}

// WischoicerWalletRechargeOptions — GET /api/wallet/recharges/options
// 返回前端可渲染的固定金额档（¥50/100/200/500）。¥1 不在此返回（仅受控测试路径可达）。
func WischoicerWalletRechargeOptions(c *gin.Context) {
	tiers := make([]walletRechargeTier, 0, len(common.WischoicerRechargeTierCents))
	var minCents, maxCents int64
	for i, cents := range common.WischoicerRechargeTierCents {
		tiers = append(tiers, walletRechargeTier{
			AmountCents: cents,
			Display:     wischoicerYuanDisplay(cents),
		})
		if i == 0 {
			minCents = cents
		}
		maxCents = cents
	}
	common.ApiSuccess(c, walletRechargeOptionsResponse{
		Currency: "CNY",
		Tiers:    tiers,
		MinCents: minCents,
		MaxCents: maxCents,
	})
}

func wischoicerYuanDisplay(cents int64) string {
	return fmt.Sprintf("¥%d", cents/100)
}

// createWalletRechargeRequest 是创建订单的浏览器请求。
// 刻意不声明任何用户归属字段——即便客户端在 JSON 里塞 newApiUserId/userId/token，gin 绑定也会忽略，
// 权威来源始终是 UserAuth context（c.GetInt("id")）。
type createWalletRechargeRequest struct {
	AmountCents     int64  `json:"amountCents"`
	ClientRequestId string `json:"clientRequestId"`
}

// CreateWischoicerWalletRecharge — POST /api/wallet/recharges
// 创建或恢复一个微信 Native 充值订单。clientRequestId 由浏览器持久化并在刷新/重试时复用。
func CreateWischoicerWalletRecharge(c *gin.Context) {
	userID := c.GetInt("id")
	if userID <= 0 {
		walletApiErrMsg(c, "请先登录后再充值")
		return
	}

	var req createWalletRechargeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		walletApiErrMsg(c, "充值请求格式有误，请刷新后重试")
		return
	}

	// clientRequestId：浏览器持久化并复用（刷新/重试同 ID）。校验长度 1–64（按 rune）。
	ridLen := len([]rune(req.ClientRequestId))
	if ridLen < 1 || ridLen > common.WischoicerRechargeClientRequestIDMaxLen {
		walletApiErrMsg(c, "充值请求标识无效，请刷新后重试")
		return
	}

	// 金额档：服务端权威校验（四档 ¥50/100/200/500；¥1 仅白名单测试账号）。
	if !common.IsWischoicerRechargeAllowedAmountCents(req.AmountCents, userID) {
		walletApiErrMsg(c, "所选金额不可用，请从给定档位中选择")
		return
	}

	order, err := walletBillingOrderClient().CreateOrder(walletContextWithRequestID(c), service.BillingCreateOrderRequest{
		NewApiUserId:    userID, // 仅来自 context，绝不来自 body
		ClientRequestId: req.ClientRequestId,
		AmountCents:     req.AmountCents,
	})
	if err != nil {
		walletApiError(c, err)
		return
	}
	common.ApiSuccess(c, projectWalletOrder(order))
}

// GetWischoicerWalletRecharge — GET /api/wallet/recharges/:orderNo
// 查询当前登录用户的订单；billing 按 newApiUserId 过滤归属，跨用户返回 OrderNotFound。
func GetWischoicerWalletRecharge(c *gin.Context) {
	userID := c.GetInt("id")
	if userID <= 0 {
		walletApiErrMsg(c, "请先登录后再查询")
		return
	}
	orderNo := c.Param("orderNo")
	if orderNo == "" {
		walletApiErrMsg(c, "订单号缺失")
		return
	}

	order, err := walletBillingOrderClient().GetOrder(walletContextWithRequestID(c), orderNo, userID)
	if err != nil {
		walletApiError(c, err)
		return
	}
	common.ApiSuccess(c, projectWalletOrder(order))
}

// walletOrderView 是投影给浏览器的安全订单视图（无 quota / 内部码 / 服务名）。
type walletOrderView struct {
	OrderNo     string `json:"orderNo"`
	AmountCents int64  `json:"amountCents"`
	Currency    string `json:"currency"`
	Status      string `json:"status"`
	CodeUrl     string `json:"codeUrl,omitempty"`
	ExpireTime  *int64 `json:"expireTime,omitempty"`
	PaidTime    *int64 `json:"paidTime,omitempty"`
}

func projectWalletOrder(o *service.BillingOrder) walletOrderView {
	if o == nil {
		return walletOrderView{Status: wischoicerWalletSafeStatus("")}
	}
	return walletOrderView{
		OrderNo:     o.OrderNo,
		AmountCents: o.AmountCents,
		Currency:    o.Currency,
		Status:      wischoicerWalletSafeStatus(o.Status),
		CodeUrl:     o.CodeUrl,
		ExpireTime:  o.ExpireTime,
		PaidTime:    o.PaidTime,
	}
}

// walletApiError 把 billing 调用 error 映射为面向用户的安全文案。
// 绝不透传 billing 的 code / message / HTTP；统一 HTTP 200 + success:false（与本仓 API 约定一致）。
func walletApiError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrBillingOrderConflict):
		walletApiErrMsg(c, "存在未支付的同类订单，请使用原订单或稍后重试")
	case errors.Is(err, service.ErrBillingCapacityExceeded):
		walletApiErrMsg(c, "账户容量已达上限，暂无法充值")
	case errors.Is(err, service.ErrBillingUserUnavailable):
		walletApiErrMsg(c, "账户当前不可用，请联系客服")
	case errors.Is(err, service.ErrBillingOrderNotFound):
		walletApiErrMsg(c, "未找到该订单")
	case errors.Is(err, service.ErrBillingInvalidArgument):
		walletApiErrMsg(c, "充值请求参数有误，请刷新后重试")
	case errors.Is(err, service.ErrBillingUnauthorized):
		// 凭据问题（ops/SRE 排查），对用户只说暂不可用，不泄露。
		walletApiErrMsg(c, "充值服务暂不可用，请稍后再试")
	default:
		// ErrBillingUnavailable / 未知：结果未知，可重试。
		walletApiErrMsg(c, "充值服务暂时不可用，请稍后重试")
	}
}

func walletApiErrMsg(c *gin.Context, msg string) {
	c.JSON(http.StatusOK, gin.H{
		"success": false,
		"message": msg,
	})
}

// walletClientOverride 允许测试注入桩 billing 客户端；生产为 nil，使用进程级默认客户端。
var walletClientOverride service.BillingRechargeOrderClient

func walletBillingOrderClient() service.BillingRechargeOrderClient {
	if walletClientOverride != nil {
		return walletClientOverride
	}
	return service.DefaultBillingRechargeOrderClient()
}

func setWischoicerWalletClientForTest(c service.BillingRechargeOrderClient) func() {
	prev := walletClientOverride
	walletClientOverride = c
	return func() { walletClientOverride = prev }
}

// walletContextWithRequestID 把 gin 中的 request id 透传到 billing 调用的 ctx，用于脱敏追踪。
// 不承载鉴权或用户身份（契约 §0）。
func walletContextWithRequestID(c *gin.Context) context.Context {
	ctx := c.Request.Context()
	if ctx.Value(common.RequestIdKey) != nil {
		return ctx
	}
	if rid := c.GetString(common.RequestIdKey); rid != "" {
		return context.WithValue(ctx, common.RequestIdKey, rid)
	}
	return ctx
}
