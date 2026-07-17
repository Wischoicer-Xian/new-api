package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests drive the §6.1 create handlers through a gin test context against
// a per-test SQLite DB. They lock the HTTP layer: the §14.1 gate, the
// Content-Type / idempotency / size guards, and the 202 Accepted shape with the
// Idempotency-Replayed header on a replay.

func setupImageTaskCreateControllerDB(t *testing.T) {
	t.Helper()
	setupImageTaskControllerDB(t)
	require.NoError(t, model.DB.AutoMigrate(
		&model.Token{},
		&model.Task{},
		&model.ChannelRevision{},
		&model.TaskBillingLedger{},
		&model.UserSubscription{},
	))
}

func enableImageTaskCreate(t *testing.T) {
	t.Helper()
	prev := constant.ImageTaskCreateEnabled
	constant.ImageTaskCreateEnabled = true
	t.Cleanup(func() { constant.ImageTaskCreateEnabled = prev })
}

// newCreateGenerationCtx builds a POST /v1/image-tasks/generations context with
// sane defaults (json Content-Type, an Idempotency-Key, owner/token/group
// injected as TokenAuth would). Callers tweak headers before invoking the handler.
func newCreateGenerationCtx(body string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/image-tasks/generations", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Idempotency-Key", "ctl-create-key")
	c.Set("id", 9001)
	c.Set("token_id", 9101)
	c.Set("group", "default")
	c.Set("user_group", "default")
	return c, w
}

// seedAcceptedImageTask wires a wallet-funded owner + token + image-capable
// channel (with ability + revision) and the model price, so a create reserve
// succeeds. The caller manages MaxImageTasksPerUser / MemoryCacheEnabled.
func seedAcceptedImageTask(t *testing.T) {
	t.Helper()
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"dall-e-3":0.04}`))
	t.Cleanup(func() { _ = ratio_setting.UpdateModelPriceByJSONString(`{}`) })
	require.NoError(t, model.DB.Create(&model.User{
		Id: 9001, Username: "ctl-owner", Quota: 1000000, Status: common.UserStatusEnabled,
	}).Error)
	require.NoError(t, model.DB.Create(&model.Token{
		Id: 9101, UserId: 9001, Key: "sk-ctl", Name: "ctl-tok",
		Status: common.TokenStatusEnabled, RemainQuota: 1000000, ExpiredTime: -1,
	}).Error)
	cfg := `{"defaults":{"generation":"sync"}}`
	require.NoError(t, model.DB.Create(&model.Channel{
		Id: 9201, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled,
		Group: "default", Models: "dall-e-3", Key: "sk-ctl-ch", ImageExecutionConfig: &cfg,
	}).Error)
	require.NoError(t, model.DB.Create(&model.Ability{Group: "default", Model: "dall-e-3", ChannelId: 9201, Enabled: true}).Error)
	require.NoError(t, model.DB.Create(&model.ChannelRevision{
		ChannelID: 9201, RevisionNumber: 1, Endpoint: "https://api.openai.com",
		CredentialRef: "channel:9201", AdapterVersion: "openai-image-adapter/v1",
	}).Error)
}

func TestCreateImageTaskGeneration_GateOffIs404(t *testing.T) {
	setupImageTaskCreateControllerDB(t)
	// Gate defaults off; do not enable it.
	c, w := newCreateGenerationCtx(`{"model":"dall-e-3","prompt":"a cat"}`)
	CreateImageTaskGeneration(c)
	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, "NOT_FOUND", parseImageTaskBody(t, w)["code"])
}

func TestCreateImageTaskGeneration_RuntimeSwitchCannotOpenRoute(t *testing.T) {
	setupImageTaskCreateControllerDB(t)
	enableImageTaskCreate(t)
	c, w := newCreateGenerationCtx(`{"model":"dall-e-3","prompt":"a cat"}`)
	CreateImageTaskGeneration(c)
	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, "NOT_FOUND", parseImageTaskBody(t, w)["code"])
}

func TestReadImageTaskRawBody_RejectsOversizedBody(t *testing.T) {
	c, _ := newCreateGenerationCtx(strings.Repeat("x", (1<<20)+1))
	_, err := readImageTaskRawBody(c)
	reqErr := dto.AsImageTaskRequestError(err)
	require.NotNil(t, reqErr)
	assert.Equal(t, http.StatusRequestEntityTooLarge, reqErr.StatusCode)
}
