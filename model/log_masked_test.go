package model

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newMaskTestContext(tokenHidden bool, requestID string) *gin.Context {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	c.Set(common.RequestIdKey, requestID)
	if tokenHidden {
		c.Set("token_hidden", true)
	}
	return c
}

// TestRecordConsumeLogMasksHiddenToken covers point 1 (logs + quota_data): when
// the request context marks the token as hidden, the consume log row stores the
// alias as ModelName and retains the real model in the admin-only
// Other.upstream_model_name (which formatUserLogs strips for non-admin queries).
func TestRecordConsumeLogMasksHiddenToken(t *testing.T) {
	const real = "claude-sonnet-4-5"
	c := newMaskTestContext(true, "mask-test-consume-hidden")

	RecordConsumeLog(c, 496001, RecordConsumeLogParams{
		ModelName: real,
		TokenId:   496001,
		Other:     map[string]interface{}{},
	})

	var log Log
	require.NoError(t, LOG_DB.First(&log, "request_id = ?", "mask-test-consume-hidden").Error)
	require.Equal(t, common.MaskedSystemModelAlias, log.ModelName)

	other, err := common.StrToMap(log.Other)
	require.NoError(t, err)
	require.Equal(t, real, other["upstream_model_name"])
}

func TestRecordConsumeLogKeepsRealModelWhenNotHidden(t *testing.T) {
	const real = "claude-sonnet-4-5"
	c := newMaskTestContext(false, "mask-test-consume-visible")

	RecordConsumeLog(c, 496002, RecordConsumeLogParams{
		ModelName: real,
		TokenId:   496002,
		Other:     map[string]interface{}{},
	})

	var log Log
	require.NoError(t, LOG_DB.First(&log, "request_id = ?", "mask-test-consume-visible").Error)
	require.Equal(t, real, log.ModelName)
	other, err := common.StrToMap(log.Other)
	require.NoError(t, err)
	_, hasAdminField := other["upstream_model_name"]
	require.False(t, hasAdminField, "non-hidden token must not stash a masked admin field")
}

// TestRecordErrorLogMasksHiddenToken covers point 2 (error logs).
func TestRecordErrorLogMasksHiddenToken(t *testing.T) {
	const real = "gemini-2.5-pro"
	c := newMaskTestContext(true, "mask-test-error-hidden")

	RecordErrorLog(c, 496003, 0, real, "tk", "boom", 0, 1, false, "g", map[string]interface{}{})

	var log Log
	require.NoError(t, LOG_DB.First(&log, "request_id = ?", "mask-test-error-hidden").Error)
	require.Equal(t, common.MaskedSystemModelAlias, log.ModelName)
	other, err := common.StrToMap(log.Other)
	require.NoError(t, err)
	require.Equal(t, real, other["upstream_model_name"])
}

// TestRecordTaskBillingLogMasksHiddenToken covers point 3 (async task billing
// logs). The token is loaded via GetTokenById inside the function; its Hidden
// flag drives the mask with no extra query. The real model is retained in the
// admin-only Other.upstream_model_name; async billing itself reads
// PrivateData.BillingContext.OriginModelName and is unaffected.
func TestRecordTaskBillingLogMasksHiddenToken(t *testing.T) {
	const real = "kling-v2-master"
	token := &Token{Id: 496010, UserId: 496010, Key: "sk-mask-hidden-496010", Name: "hidden-sys", Hidden: true, Status: 1, ExpiredTime: -1}
	require.NoError(t, DB.Create(token).Error)
	t.Cleanup(func() {
		DB.Unscoped().Delete(token)
		DB.Unscoped().Where("token_id = ?", 496010).Delete(&Log{})
	})

	RecordTaskBillingLog(RecordTaskBillingLogParams{
		UserId:    token.UserId,
		LogType:   LogTypeRefund,
		ModelName: real,
		TokenId:   496010,
		Other:     map[string]interface{}{},
	})

	var log Log
	require.NoError(t, LOG_DB.First(&log, "token_id = ?", 496010).Error)
	require.Equal(t, common.MaskedSystemModelAlias, log.ModelName)
	other, err := common.StrToMap(log.Other)
	require.NoError(t, err)
	require.Equal(t, real, other["upstream_model_name"])
}

func TestRecordTaskBillingLogKeepsRealModelWhenNotHidden(t *testing.T) {
	const real = "kling-v2-master"
	token := &Token{Id: 496020, UserId: 496020, Key: "sk-mask-visible-496020", Name: "normal", Hidden: false, Status: 1, ExpiredTime: -1}
	require.NoError(t, DB.Create(token).Error)
	t.Cleanup(func() {
		DB.Unscoped().Delete(token)
		DB.Unscoped().Where("token_id = ?", 496020).Delete(&Log{})
	})

	RecordTaskBillingLog(RecordTaskBillingLogParams{
		UserId:    token.UserId,
		LogType:   LogTypeRefund,
		ModelName: real,
		TokenId:   496020,
		Other:     map[string]interface{}{},
	})

	var log Log
	require.NoError(t, LOG_DB.First(&log, "token_id = ?", 496020).Error)
	require.Equal(t, real, log.ModelName)
}
