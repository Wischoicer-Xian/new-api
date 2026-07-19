package controller

import (
	"errors"
	"net/http"

	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// wischoicerBillingAudit 记录 wischoicer-billing → new-api 内部接口的操作审计。
// operator 为目标 new-api user id（billing 内部接口无登录管理员）；source 标记调用方。
func wischoicerBillingAudit(c *gin.Context, action string, targetUserId int, params map[string]interface{}) {
	if params == nil {
		params = map[string]interface{}{}
	}
	params["source"] = "wischoicer-billing"
	params["client_ip"] = c.ClientIP()
	model.RecordOperationAuditLog(targetUserId, action, c.ClientIP(), action, params, nil, nil)
}

// ---------------------------------------------------------------------------
// Wischoicer 充值容量预留 / 幂等入账 内部接口（方案 §7.4/7.5）
// ---------------------------------------------------------------------------

type reserveRechargeRequest struct {
	NewApiUserId    int    `json:"newApiUserId" binding:"required"`
	Quota           int    `json:"quota" binding:"required"`
	AmountCents     int64  `json:"amountCents" binding:"required"`
	Currency        string `json:"currency" binding:"required"`
	PaymentProvider string `json:"paymentProvider" binding:"required"`
}

type reserveRechargeResponse struct {
	Reserved  bool `json:"reserved"`
	Duplicate bool `json:"duplicate"`
}

// PutWischoicerRechargeReservation — PUT /api/internal/wischoicer/recharge-reservations/{orderNo}
func PutWischoicerRechargeReservation(c *gin.Context) {
	orderNo := c.Param("orderNo")
	if orderNo == "" {
		wischoicerCreditErrorResponse(c, http.StatusBadRequest, "INVALID_ARGUMENT")
		return
	}

	var req reserveRechargeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		wischoicerCreditErrorResponse(c, http.StatusBadRequest, "INVALID_ARGUMENT")
		return
	}

	result, err := model.ReserveExternalRecharge(c.Request.Context(), model.ReserveExternalRechargeRequest{
		OrderNo:         orderNo,
		NewApiUserId:    req.NewApiUserId,
		Quota:           req.Quota,
		AmountCents:     req.AmountCents,
		Currency:        req.Currency,
		PaymentProvider: req.PaymentProvider,
	})
	if err != nil {
		wischoicerCreditErrorResponse(c, wischoicerHTTPStatusForError(err), err.Error())
		return
	}

	wischoicerBillingAudit(c, "wischoicer.recharge.reserve", req.NewApiUserId, map[string]interface{}{
		"order_no":     orderNo,
		"quota":        req.Quota,
		"amount_cents": req.AmountCents,
		"reserved":     result.Reserved,
		"duplicate":    result.Duplicate,
	})

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": reserveRechargeResponse{
			Reserved:  result.Reserved,
			Duplicate: result.Duplicate,
		},
	})
}

type releaseRechargeRequest struct {
	Reason string `json:"reason"`
}

// wischoicerValidReleaseReasons 是 release reason 的受控枚举（WIS-547 §2）。
// 依据 billing develop actual enum（当前仅发 "closed"）+ 契约显式枚举（closed/expired/prepay_failed）
// 收口；新增 reason 必须先扩此集合，禁止任意字符串（R2 复审 P2）。
var wischoicerValidReleaseReasons = map[string]struct{}{
	"closed":        {},
	"expired":       {},
	"prepay_failed": {},
}

func isWischoicerValidReleaseReason(reason string) bool {
	_, ok := wischoicerValidReleaseReasons[reason]
	return ok
}

// PostWischoicerRechargeReservationRelease — POST /api/internal/wischoicer/recharge-reservations/{orderNo}/release
func PostWischoicerRechargeReservationRelease(c *gin.Context) {
	orderNo := c.Param("orderNo")
	if orderNo == "" {
		wischoicerCreditErrorResponse(c, http.StatusBadRequest, "INVALID_ARGUMENT")
		return
	}

	var req releaseRechargeRequest
	// body 必填（契约 §2：{reason}）；绑定失败或 reason 不在受控枚举 → 400 INVALID_ARGUMENT。
	if err := c.ShouldBindJSON(&req); err != nil {
		wischoicerCreditErrorResponse(c, http.StatusBadRequest, "INVALID_ARGUMENT")
		return
	}
	if !isWischoicerValidReleaseReason(req.Reason) {
		wischoicerCreditErrorResponse(c, http.StatusBadRequest, "INVALID_ARGUMENT")
		return
	}

	if err := model.ReleaseExternalRecharge(c.Request.Context(), orderNo, req.Reason); err != nil {
		wischoicerCreditErrorResponse(c, wischoicerHTTPStatusForError(err), err.Error())
		return
	}

	wischoicerBillingAudit(c, "wischoicer.recharge.release", 0, map[string]interface{}{
		"order_no": orderNo,
		"reason":   req.Reason,
	})

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    gin.H{"released": true},
	})
}

type creditRechargeRequest struct {
	OrderNo         string `json:"orderNo" binding:"required"`
	NewApiUserId    int    `json:"newApiUserId" binding:"required"`
	Quota           int    `json:"quota" binding:"required"`
	AmountCents     int64  `json:"amountCents" binding:"required"`
	Currency        string `json:"currency" binding:"required"`
	PaymentProvider string `json:"paymentProvider" binding:"required"`
	TransactionId   string `json:"transactionId" binding:"required"`
	PaidAt          int64  `json:"paidAt" binding:"required"`
}

type creditRechargeResponse struct {
	Credited  bool   `json:"credited"`
	Duplicate bool   `json:"duplicate"`
	OrderNo   string `json:"orderNo"`
}

// PostWischoicerRechargeCredit — POST /api/internal/wischoicer/recharge-credits
func PostWischoicerRechargeCredit(c *gin.Context) {
	var req creditRechargeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		wischoicerCreditErrorResponse(c, http.StatusBadRequest, "INVALID_ARGUMENT")
		return
	}

	result, err := model.CreditExternalRecharge(c.Request.Context(), model.CreditExternalRechargeRequest{
		OrderNo:         req.OrderNo,
		NewApiUserId:    req.NewApiUserId,
		Quota:           req.Quota,
		AmountCents:     req.AmountCents,
		Currency:        req.Currency,
		PaymentProvider: req.PaymentProvider,
		TransactionId:   req.TransactionId,
		PaidAt:          req.PaidAt,
	})
	if err != nil {
		wischoicerCreditErrorResponse(c, wischoicerHTTPStatusForError(err), err.Error())
		return
	}

	wischoicerBillingAudit(c, "wischoicer.recharge.credit", req.NewApiUserId, map[string]interface{}{
		"order_no":       req.OrderNo,
		"quota":          req.Quota,
		"amount_cents":   req.AmountCents,
		"transaction_id": req.TransactionId,
		"credited":       result.Credited,
		"duplicate":      result.Duplicate,
	})

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": creditRechargeResponse{
			Credited:  result.Credited,
			Duplicate: result.Duplicate,
			OrderNo:   result.OrderNo,
		},
	})
}

type getRechargeCreditResponse struct {
	OrderNo       string `json:"orderNo"`
	Status        string `json:"status"`
	CacheStatus   string `json:"cacheStatus"`
	TransactionId string `json:"transactionId"`
}

// GetWischoicerRechargeCredit — GET /api/internal/wischoicer/recharge-credits/{orderNo}
func GetWischoicerRechargeCredit(c *gin.Context) {
	orderNo := c.Param("orderNo")
	if orderNo == "" {
		wischoicerCreditErrorResponse(c, http.StatusBadRequest, "INVALID_ARGUMENT")
		return
	}

	credit, err := model.GetWischoicerRechargeCredit(c.Request.Context(), orderNo)
	if err != nil {
		wischoicerCreditErrorResponse(c, wischoicerHTTPStatusForError(err), err.Error())
		return
	}

	transactionId := ""
	if credit.ExternalTransactionId != nil {
		transactionId = *credit.ExternalTransactionId
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": getRechargeCreditResponse{
			OrderNo:       credit.OrderNo,
			Status:        wischoicerCreditStatusLabel(credit.Status),
			CacheStatus:   wischoicerCacheStatusLabel(credit.CacheStatus),
			TransactionId: transactionId,
		},
	})
}

// ---------------------------------------------------------------------------
// 错误映射
// ---------------------------------------------------------------------------

// wischoicerHTTPStatusForError 把 model 层哨兵错误映射到方案 §10 的 HTTP code。
func wischoicerHTTPStatusForError(err error) int {
	switch {
	case errors.Is(err, model.ErrWischoicerInvalidArgument):
		return http.StatusBadRequest
	case errors.Is(err, model.ErrWischoicerCreditNotFound):
		return http.StatusNotFound
	case errors.Is(err, model.ErrWischoicerCreditUserUnavailable):
		return http.StatusConflict
	case errors.Is(err, model.ErrWischoicerReservationConflict):
		return http.StatusConflict
	case errors.Is(err, model.ErrWischoicerQuotaCapacityExceeded):
		return http.StatusConflict
	case errors.Is(err, model.ErrWischoicerCreditConflict):
		return http.StatusConflict
	case errors.Is(err, model.ErrWischoicerReservationReleased):
		return http.StatusConflict
	case errors.Is(err, gorm.ErrRecordNotFound):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}

// wischoicerCreditErrorResponse 统一输出 { success:false, code, message }。
func wischoicerCreditErrorResponse(c *gin.Context, status int, code string) {
	c.JSON(status, gin.H{
		"success": false,
		"code":    code,
		"message": code,
	})
}

func wischoicerCreditStatusLabel(status int) string {
	switch status {
	case model.WischoicerCreditStatusReserved:
		return "RESERVED"
	case model.WischoicerCreditStatusSuccess:
		return "SUCCESS"
	case model.WischoicerCreditStatusReleased:
		return "RELEASED"
	default:
		return "UNKNOWN"
	}
}

func wischoicerCacheStatusLabel(status int) string {
	switch status {
	case model.WischoicerCacheStatusPending:
		return "PENDING"
	case model.WischoicerCacheStatusVerifyPending:
		return "VERIFY_PENDING"
	case model.WischoicerCacheStatusSuccess:
		return "SUCCESS"
	default:
		return "UNKNOWN"
	}
}
