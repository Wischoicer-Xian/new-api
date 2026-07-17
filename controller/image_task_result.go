package controller

import (
	"errors"
	"net/http"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// GetImageTaskResult serves the durable result bytes (§7.6) for a completed
// image task. The processor persisted the bytes in ImageTaskResultBlob so the
// result survives the upstream download_url expiry. Owner-scoped like GET/cancel
// (§6.1): a missing task or result is 404 so no cross-account existence leaks.
//
// Clients reach this via the ImageTaskResult.content_url locator, so it is
// served as raw image bytes with the stored MIME type — not as a JSON envelope.
func GetImageTaskResult(c *gin.Context) {
	ownerUserID := c.GetInt("id")
	publicTaskID := c.Param("task_id")
	exec, err := model.GetImageTaskExecutionByPublicTaskID(publicTaskID, ownerUserID)
	if err != nil {
		writeImageTaskError(c, mapImageTaskResultLookupErr(err, "image task not found"))
		return
	}
	blob, err := model.GetImageTaskResultBlob(exec.ID)
	if err != nil {
		writeImageTaskError(c, mapImageTaskResultLookupErr(err, "image task result not available"))
		return
	}
	contentType := blob.MimeType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	c.Data(http.StatusOK, contentType, blob.Content)
}

// mapImageTaskResultLookupErr folds a model lookup error into the §6.1 error
// envelope: a missing row is NOT_FOUND (no cross-account leak), any other DB
// error is returned as-is so writeImageTaskError reports it as a generic 500.
func mapImageTaskResultLookupErr(err error, missingMsg string) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return &dto.ImageTaskRequestError{
			Code:       dto.ImageTaskErrNotFound,
			StatusCode: http.StatusNotFound,
			Message:    missingMsg,
		}
	}
	return err
}
