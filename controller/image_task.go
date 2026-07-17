package controller

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
)

// GetImageTask handles GET /v1/image-tasks/:task_id. It reads the durable
// image-task state scoped to the authenticated owner and projects it onto the
// §6.1 public object. A missing or cross-account task returns 404; a
// manual_review task carries a long Retry-After (§6.1).
func GetImageTask(c *gin.Context) {
	ownerUserID := c.GetInt("id")
	publicTaskID := c.Param("task_id")
	obj, retryAfter, err := service.GetImageTask(ownerUserID, publicTaskID)
	if err != nil {
		writeImageTaskError(c, err)
		return
	}
	if retryAfter > 0 {
		c.Header("Retry-After", strconv.Itoa(retryAfter))
	}
	c.JSON(http.StatusOK, obj)
}

// CancelImageTask handles POST /v1/image-tasks/:task_id/cancel. It records a
// cancel request on a non-terminal task; the processor performs the actual
// provider cancel and terminal settle/refund (§9.2 cancel_guard). Terminal and
// already-cancel-requested tasks are an idempotent no-op — the response always
// shows the current public state.
func CancelImageTask(c *gin.Context) {
	ownerUserID := c.GetInt("id")
	publicTaskID := c.Param("task_id")
	obj, _, err := service.CancelImageTask(ownerUserID, publicTaskID, time.Now().Unix())
	if err != nil {
		writeImageTaskError(c, err)
		return
	}
	c.JSON(http.StatusOK, obj)
}

// CreateImageTaskGeneration handles POST /v1/image-tasks/generations — the
// §6.1 async single-image generation entry point. It strict-decodes the body,
// resolves a channel and price, and atomically reserves a task, responding 202
// Accepted with the public projection. The route is gated by the §14.1
// create-allowlist switch; with it off the route fails closed as 404.
func CreateImageTaskGeneration(c *gin.Context) {
	createImageTask(c, service.ImageOperationGeneration)
}

// CreateImageTaskEdit handles POST /v1/image-tasks/edits — the §6.1 async edit
// entry point. It runs the same flow as generation against the edit operation.
func CreateImageTaskEdit(c *gin.Context) {
	createImageTask(c, service.ImageOperationEdit)
}

// createImageTask is the shared create handler body. The raw body is read once
// and passed to the service so the canonical hash and the strict decode share
// identical bytes. Idempotency-Replayed is set on a reserve replay so a client
// can tell its accepted task is the original rather than a fresh one.
func createImageTask(c *gin.Context, operation service.ImageOperation) {
	if !service.ImageTaskCreateAllowed() {
		writeImageTaskError(c, &dto.ImageTaskRequestError{
			Code:       dto.ImageTaskErrNotFound,
			StatusCode: http.StatusNotFound,
			Message:    "image task create is not available",
		})
		return
	}
	if err := dto.ValidateImageTaskContentType(c.GetHeader("Content-Type")); err != nil {
		writeImageTaskError(c, err)
		return
	}
	rawBody, err := readImageTaskRawBody(c)
	if err != nil {
		writeImageTaskError(c, err)
		return
	}
	idempotencyKey := c.GetHeader("Idempotency-Key")
	if err := dto.ValidateIdempotencyKey(idempotencyKey); err != nil {
		writeImageTaskError(c, err)
		return
	}
	input := service.ImageTaskCreateInput{
		RawBody:         rawBody,
		Operation:       operation,
		OwnerUserID:     c.GetInt("id"),
		CreationTokenID: c.GetInt("token_id"),
		IdempotencyKey:  idempotencyKey,
		UsingGroup:      common.GetContextKeyString(c, constant.ContextKeyUsingGroup),
		UserBaseGroup:   common.GetContextKeyString(c, constant.ContextKeyUserGroup),
		RequestID:       c.GetString(common.RequestIdKey),
	}
	if attr := common.ParseWischoicerAttribution(c.Request.Header); attr != nil {
		if payload, mErr := common.Marshal(attr); mErr == nil {
			input.Attribution = payload
		}
	}
	obj, replayed, _, sErr := service.CreateImageTask(c.Request.Context(), input)
	if sErr != nil {
		writeImageTaskError(c, sErr)
		return
	}
	if replayed {
		c.Header("Idempotency-Replayed", "true")
	}
	c.JSON(http.StatusAccepted, obj)
}

// readImageTaskRawBody reads the request body once via the shared BodyStorage
// so the same bytes feed both the canonical hash and the strict decode. A
// missing or unreadable body is a 400, not a 500.
func readImageTaskRawBody(c *gin.Context) ([]byte, error) {
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return nil, &dto.ImageTaskRequestError{
			Code:       dto.ImageTaskErrInvalidRequest,
			StatusCode: http.StatusBadRequest,
			Message:    "request body is required",
		}
	}
	body, err := storage.Bytes()
	if err != nil || len(body) == 0 {
		return nil, &dto.ImageTaskRequestError{
			Code:       dto.ImageTaskErrInvalidRequest,
			StatusCode: http.StatusBadRequest,
			Message:    "request body is required",
		}
	}
	return body, nil
}

// writeImageTaskError maps a service error onto the §6.1 error response. An
// ImageTaskRequestError carries its own status, code, message and optional
// Retry-After; anything else is reported as an internal error so an unexpected
// failure never leaks a raw message or an internal state name.
func writeImageTaskError(c *gin.Context, err error) {
	if reqErr := dto.AsImageTaskRequestError(err); reqErr != nil {
		if reqErr.RetryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(reqErr.RetryAfter))
		}
		c.JSON(reqErr.StatusCode, gin.H{
			"code":    string(reqErr.Code),
			"message": reqErr.Message,
		})
		return
	}
	logger.LogError(c.Request.Context(), fmt.Sprintf("image-task handler unexpected error: %v", err))
	c.JSON(http.StatusInternalServerError, gin.H{
		"code":    "INTERNAL_ERROR",
		"message": "internal error",
	})
}
