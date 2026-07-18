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

// setImageTaskReleaseGate sets the §14.1 create + processor release switches
// and restores their previous values when the test ends. Tests set these
// post-init to drive the gate function directly; the startup gate
// (common/init.go) only enforces legal combinations at boot, so this does not
// re-trigger it.
func setImageTaskReleaseGate(t *testing.T, create, processor bool) {
	t.Helper()
	prevCreate := constant.ImageTaskCreateEnabled
	prevProc := constant.ImageTaskProcessorEnabled
	constant.ImageTaskCreateEnabled = create
	constant.ImageTaskProcessorEnabled = processor
	t.Cleanup(func() {
		constant.ImageTaskCreateEnabled = prevCreate
		constant.ImageTaskProcessorEnabled = prevProc
	})
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

// TestCreateImageTaskGeneration_ReleaseGateMatrix locks the §14.1 release gate at
// the HTTP layer across the four create×processor combinations. The three closed
// combinations stay 404 NOT_FOUND with the create-not-available message; only
// on/on admits the request. Admission is proven by reaching the next guard — an
// invalid Content-Type yields 415 UNSUPPORTED_MEDIA_TYPE rather than the gate's
// 404 — instead of asserting a vague "not 404".
func TestCreateImageTaskGeneration_ReleaseGateMatrix(t *testing.T) {
	cases := []struct {
		name                  string
		create, processor     bool
		contentType           string
		wantStatus            int
		wantCode, wantMessage string
	}{
		{"off/off 404", false, false, "application/json", http.StatusNotFound, "NOT_FOUND", "image task create is not available"},
		{"off/on 404", false, true, "application/json", http.StatusNotFound, "NOT_FOUND", "image task create is not available"},
		{"on/off 404", true, false, "application/json", http.StatusNotFound, "NOT_FOUND", "image task create is not available"},
		{"on/on admitted, reaches Content-Type guard", true, true, "text/plain", http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setupImageTaskCreateControllerDB(t)
			setImageTaskReleaseGate(t, tc.create, tc.processor)

			c, w := newCreateGenerationCtx(`{"model":"dall-e-3","prompt":"a cat"}`)
			c.Request.Header.Set("Content-Type", tc.contentType)
			CreateImageTaskGeneration(c)

			require.Equal(t, tc.wantStatus, w.Code, "status")
			m := parseImageTaskBody(t, w)
			assert.Equal(t, tc.wantCode, m["code"], "code")
			assert.Equal(t, tc.wantMessage, m["message"], "message")
		})
	}
}

func TestReadImageTaskRawBody_RejectsOversizedBody(t *testing.T) {
	c, _ := newCreateGenerationCtx(strings.Repeat("x", (1<<20)+1))
	_, err := readImageTaskRawBody(c)
	reqErr := dto.AsImageTaskRequestError(err)
	require.NotNil(t, reqErr)
	assert.Equal(t, http.StatusRequestEntityTooLarge, reqErr.StatusCode)
}
