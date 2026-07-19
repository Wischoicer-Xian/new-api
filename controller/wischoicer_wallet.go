package controller

import (
	"errors"
	"net/http"
	"strconv"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
)

// ---------------------------------------------------------------------------
// Wischoicer 钱包 UserAuth façade —— 浏览器 ↔ new-api 钱包（Token A 下游）
// ---------------------------------------------------------------------------
//
// 浏览器只和这个 façade 说话。它从 **UserAuth context** 取 numeric user ID，
// 绝不从 request body/header 接收 newApiUserId/user_id；然后以服务身份调用 billing
// 内部订单接口（Token A）。返回给浏览器的只有安全字段：人民币金额、orderNo、面向
// 用户的状态、codeUrl、expireTime——绝不返回 quota、内部错误码、服务名或 token。
//
// 安全边界（WIS-547 契约 §1/§3）：
//   - newApiUserID 仅来自 c.GetInt("id")（UserAuth）。
//   - 金额档位由服务端权威门禁（common.IsWischoicerRechargeAmountAllowed）；
//     前端无法改金额/quota/用户归属，绕过前端直接构造越界金额一律被拒。
//   - billing 内部错误码一律映射为用户文案，不透传 code/HTTP/quota/token。

// wischoicerBillingClientInstance 是钱包 façade 使用的 billing Token A client
// （包级单例，E2E 依赖 S3 端点就绪；测试可经 injectMockBillingClient 注入）。
// wischoicerBillingClientMu 保护它的惰性初始化与测试注入：生产并发首次调用只创建
// 一次，读用 RLock、写（初始化 / 注入）用 Lock，杜绝读/写竞争（WIS-550 R3 race 返修）。
var (
	wischoicerBillingClientMu       sync.RWMutex
	wischoicerBillingClientInstance service.WischoicerBillingClient
)

func getWischoicerBillingClient() service.WischoicerBillingClient {
	wischoicerBillingClientMu.RLock()
	c := wischoicerBillingClientInstance
	wischoicerBillingClientMu.RUnlock()
	if c != nil {
		return c
	}

	wischoicerBillingClientMu.Lock()
	defer wischoicerBillingClientMu.Unlock()
	// 拿到写锁后必须复查：等待期间可能已有别的 goroutine 完成初始化。
	if wischoicerBillingClientInstance != nil {
		return wischoicerBillingClientInstance
	}
	wischoicerBillingClientInstance = service.NewWischoicerBillingClient()
	return wischoicerBillingClientInstance
}

// walletRechargeView 是投影给浏览器的安全订单视图。不含 quota、内部状态码、
// 服务名或 token。
type walletRechargeView struct {
	OrderNo     string `json:"orderNo"`
	AmountCents int64  `json:"amountCents"`
	Currency    string `json:"currency"`
	Status      string `json:"status"`
	CodeURL     string `json:"codeUrl,omitempty"`
	ExpireTime  int64  `json:"expireTime,omitempty"`
	// PaidTime 是订单支付时间（Unix 秒），billing 已支付订单才带；契约 §1 允许的安全
	// 字段，供钱包前端展示「支付时间」。未支付为 0，omitempty 自动省略。
	PaidTime int64 `json:"paidTime,omitempty"`
}

func projectWalletRechargeView(o *service.BillingRechargeOrder) walletRechargeView {
	if o == nil {
		return walletRechargeView{}
	}
	return walletRechargeView{
		OrderNo:     o.OrderNo,
		AmountCents: o.AmountCents,
		Currency:    o.Currency,
		Status:      o.Status,
		CodeURL:     o.CodeURL,
		ExpireTime:  o.ExpireTime,
		PaidTime:    o.PaidTime,
	}
}

type createWalletRechargeRequest struct {
	// ClientRequestID 是钱包侧幂等键（可选）。为空时由 façade 生成；浏览器重试必须复用
	// 同一值，billing 按 (newApiUserId, clientRequestId) 返回原订单。它不是身份字段。
	ClientRequestID string `json:"clientRequestId"`
	// AmountCents 是人民币分。仅接受服务端档位（¥50/100/200/500），¥1 仅白名单测试用户。
	AmountCents int64 `json:"amountCents"`
	// 注意：故意不绑定 newApiUserId / user_id —— 用户归属只从 UserAuth context 派生。
}

const wischoicerClientRequestIDMaxLen = 64

// CreateWischoicerWalletRecharge — POST /api/wallet/recharges
//
// 创建或重取充值订单。newApiUserID 仅来自 UserAuth context；金额档位服务端门禁；
// clientRequestId 缺省时生成。响应只投影安全字段。
func CreateWischoicerWalletRecharge(c *gin.Context) {
	userID := c.GetInt("id")
	if userID <= 0 {
		// UserAuth 中间件已保证 id 非空；走到这里说明鉴权异常，fail-closed。
		walletErrorResponse(c, http.StatusUnauthorized, "请先登录")
		return
	}

	var req createWalletRechargeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		walletErrorResponse(c, http.StatusBadRequest, "充值请求参数有误，请刷新重试")
		return
	}

	// 服务端金额档位门禁：拒绝越界金额（含普通用户构造 amountCents=100、或自定义金额）。
	if !common.IsWischoicerRechargeAmountAllowed(req.AmountCents, userID) {
		walletErrorResponse(c, http.StatusBadRequest, "充值金额不在可选档位内")
		return
	}

	// 幂等键：缺省生成；浏览器重试复用同一值。
	clientRequestID := req.ClientRequestID
	if clientRequestID == "" {
		clientRequestID = common.GetUUID()
	} else if len(clientRequestID) > wischoicerClientRequestIDMaxLen {
		walletErrorResponse(c, http.StatusBadRequest, "充值请求参数有误，请刷新重试")
		return
	}

	order, err := getWischoicerBillingClient().CreateRechargeOrder(c.Request.Context(), service.BillingCreateOrderRequest{
		NewApiUserID:    userID, // 仅来自 UserAuth
		ClientRequestID: clientRequestID,
		AmountCents:     req.AmountCents,
	})
	if err != nil {
		walletRespondBillingError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    projectWalletRechargeView(order),
	})
}

// GetWischoicerWalletRecharge — GET /api/wallet/recharges/:orderNo
//
// 查单。newApiUserID 仅来自 UserAuth；billing 侧按归属过滤，跨用户查单返回
// ORDER_NOT_FOUND（防枚举）。响应只投影安全字段。
func GetWischoicerWalletRecharge(c *gin.Context) {
	userID := c.GetInt("id")
	if userID <= 0 {
		walletErrorResponse(c, http.StatusUnauthorized, "请先登录")
		return
	}
	orderNo := c.Param("orderNo")
	if orderNo == "" {
		walletErrorResponse(c, http.StatusBadRequest, "订单参数有误")
		return
	}

	order, err := getWischoicerBillingClient().GetRechargeOrder(c.Request.Context(), orderNo, userID)
	if err != nil {
		walletRespondBillingError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    projectWalletRechargeView(order),
	})
}

// ListWischoicerWalletRecharges — GET /api/wallet/recharges?cursor=&limit=
//
// 订单历史/恢复。newApiUserID 仅来自 UserAuth；limit 钳制到 1–50。
func ListWischoicerWalletRecharges(c *gin.Context) {
	userID := c.GetInt("id")
	if userID <= 0 {
		walletErrorResponse(c, http.StatusUnauthorized, "请先登录")
		return
	}
	cursor := c.Query("cursor")
	limit := 20
	if v := c.Query("limit"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			limit = parsed
		}
	}

	result, err := getWischoicerBillingClient().ListRechargeOrders(c.Request.Context(), userID, cursor, limit)
	if err != nil {
		walletRespondBillingError(c, err)
		return
	}
	items := make([]walletRechargeView, 0, len(result.Items))
	for i := range result.Items {
		items = append(items, projectWalletRechargeView(&result.Items[i]))
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"items":      items,
			"nextCursor": result.NextCursor,
		},
	})
}

// ---------------------------------------------------------------------------
// 错误映射：billing 内部错误码 → 用户安全文案 + 粗粒度 HTTP 状态
// ---------------------------------------------------------------------------

// walletRespondBillingError 把 billing 错误映射为面向用户的安全响应。
// 绝不透传 billing code、message、quota、服务名或 token；只返回用户文案。
func walletRespondBillingError(c *gin.Context, err error) {
	var berr *service.BillingRechargeError
	if errors.As(err, &berr) {
		// UNAUTHORIZED / 传输错误属于服务侧异常：对用户表现为「暂不可用」，
		// 不暴露鉴权细节；内部记 SysError 供 SRE 排查（绝不记 token）。
		if berr.Code == service.BillingErrUnauthorized || berr.Retryable {
			common.SysError("wischoicer wallet billing unavailable: code=" + berr.Code +
				" http=" + strconv.Itoa(berr.HTTPStatus) + " user=" + strconv.Itoa(c.GetInt("id")))
			walletErrorResponse(c, http.StatusServiceUnavailable, "充值服务暂不可用，请稍后再试")
			return
		}
		httpStatus, msg := walletUserMessageForCode(berr.Code)
		c.JSON(httpStatus, gin.H{
			"success": false,
			"message": msg,
		})
		return
	}
	// 非 *BillingRechargeError：未预期错误，不泄露细节。
	common.SysError("wischoicer wallet unexpected error: " + err.Error())
	walletErrorResponse(c, http.StatusInternalServerError, "充值失败，请稍后再试")
}

// walletUserMessageForCode 把稳定错误码映射为 (HTTP 状态, 用户文案)。
func walletUserMessageForCode(code string) (int, string) {
	switch code {
	case service.BillingErrInvalidArgument:
		return http.StatusBadRequest, "充值金额或参数有误，请刷新重试"
	case service.BillingErrOrderNotFound:
		return http.StatusNotFound, "订单不存在"
	case service.BillingErrOrderConflict:
		return http.StatusConflict, "存在一笔相同金额的未支付订单，请刷新查看或使用原订单"
	case service.BillingErrQuotaCapacityExceeded:
		return http.StatusConflict, "已达账户额度上限，请消耗后再充值或联系客服"
	case service.BillingErrCreditUserUnavailable:
		return http.StatusForbidden, "账户当前不可用，请联系客服"
	default:
		return http.StatusInternalServerError, "充值失败，请稍后再试"
	}
}

// walletErrorResponse 输出统一的安全错误包络。
func walletErrorResponse(c *gin.Context, status int, message string) {
	c.JSON(status, gin.H{
		"success": false,
		"message": message,
	})
}
