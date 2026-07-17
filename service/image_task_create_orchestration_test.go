package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupCreateTest gives each create-orchestration test a clean SQLite state with
// the DB channel path active (MemoryCacheEnabled=false) and the pricing maps
// restored afterwards. It also pins a positive in-flight cap: the test binary
// does not run common.init's env parse, so MaxImageTasksPerUser stays at its
// zero default and every reserve would otherwise hit the cap.
func setupCreateTest(t *testing.T) {
	t.Helper()
	truncate(t)
	prev := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = prev })
	prevCap := constant.MaxImageTasksPerUser
	constant.MaxImageTasksPerUser = constant.DefaultMaxImageTasksPerUser
	t.Cleanup(func() { constant.MaxImageTasksPerUser = prevCap })
	saveRestorePriceMaps(t)
}

func seedTokenForImage(t *testing.T, id, userId int, key string, remain int) {
	t.Helper()
	require.NoError(t, model.DB.Create(&model.Token{
		Id:          id,
		UserId:      userId,
		Key:         key,
		Name:        "img-tok",
		Status:      common.TokenStatusEnabled,
		RemainQuota: remain,
		ExpiredTime: -1, // never expires; lockAndValidateToken requires -1 or > now
	}).Error)
}

// seedImageChannelForCreate persists an OpenAI image-capable channel, its
// ability, and one immutable revision so the full create path can select it.
func seedImageChannelForCreate(t *testing.T, id int, group, modelName, cfg string) {
	t.Helper()
	require.NoError(t, model.DB.Create(&model.Channel{
		Id:                   id,
		Type:                 constant.ChannelTypeOpenAI,
		Status:               common.ChannelStatusEnabled,
		Group:                group,
		Models:               modelName,
		Key:                  "sk-img",
		ImageExecutionConfig: &cfg,
	}).Error)
	require.NoError(t, model.DB.Create(&model.Ability{
		Group: group, Model: modelName, ChannelId: id, Enabled: true,
	}).Error)
	require.NoError(t, model.DB.Create(&model.ChannelRevision{
		ChannelID:      id,
		RevisionNumber: 1,
		Endpoint:       "https://api.openai.com",
		CredentialRef:  fmt.Sprintf("channel:%d", id),
		AdapterVersion: "openai-image-adapter/v1",
		Settings:       mustImageRevisionSettings(t, cfg),
	}).Error)
}

func mustImageRevisionSettings(t *testing.T, executionConfig string) []byte {
	t.Helper()
	payload, err := common.Marshal(map[string]any{
		"schema_version":   1,
		"execution_config": executionConfig,
	})
	require.NoError(t, err)
	return payload
}

func TestMapImageTaskReserveError(t *testing.T) {
	cases := []struct {
		name       string
		in         error
		status     int
		code       dto.ImageTaskErrorCode
		retryAfter int
	}{
		{"idempotency conflict", model.ErrImageTaskIdempotencyConflict, 409, dto.ImageTaskErrIdempotencyConflict, 0},
		{"in-flight cap", model.ErrImageTaskInFlightCapReached, 429, dto.ImageTaskErrTooManyRequests, 1},
		{"wallet insufficient", model.ErrImageTaskWalletInsufficient, 402, dto.ImageTaskErrInsufficientQuota, 0},
		{"subscription insufficient", model.ErrImageTaskSubscriptionInsufficient, 402, dto.ImageTaskErrInsufficientQuota, 0},
		{"no active subscription", model.ErrImageTaskNoActiveSubscription, 402, dto.ImageTaskErrInsufficientQuota, 0},
		{"token invalid", model.ErrImageTaskTokenInvalid, 401, dto.ImageTaskErrUnauthorized, 0},
		{"pricing facts", model.ErrUnsupportedImageTaskPricingFacts, 500, dto.ImageTaskErrInternal, 0},
		{"billing data", model.ErrImageTaskBillingData, 500, dto.ImageTaskErrInternal, 0},
		{"billing retry exhausted", model.ErrImageTaskBillingRetryExhausted, 500, dto.ImageTaskErrInternal, 0},
		{"cache safety", model.ErrImageTaskCacheSafetyMisconfigured, 503, dto.ImageTaskErrServiceUnavailable, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mapped := mapImageTaskReserveError(tc.in)
			reqErr := dto.AsImageTaskRequestError(mapped)
			require.NotNil(t, reqErr)
			assert.Equal(t, tc.status, reqErr.StatusCode)
			assert.Equal(t, tc.code, reqErr.Code)
			assert.Equal(t, tc.retryAfter, reqErr.RetryAfter)
		})
	}
	// Unknown errors pass through unchanged so writeImageTaskError reports them
	// as a generic 500 instead of masquerading as a mapped code.
	unknown := errors.New("boom")
	assert.Same(t, unknown, mapImageTaskReserveError(unknown))
}

func TestWeightedImageTaskSelection_HonorsPriorityAndWeight(t *testing.T) {
	priorityHigh, priorityLow := int64(10), int64(1)
	weightOne, weightNine := uint(1), uint(9)
	candidates := []imageTaskChannelSelection{
		{Channel: &model.Channel{Id: 1, Priority: &priorityLow, Weight: &weightNine}},
		{Channel: &model.Channel{Id: 2, Priority: &priorityHigh, Weight: &weightOne}},
		{Channel: &model.Channel{Id: 3, Priority: &priorityHigh, Weight: &weightNine}},
	}
	assert.Equal(t, 2, weightedImageTaskSelection(candidates, func(int) int { return 0 }).Channel.Id)
	assert.Equal(t, 3, weightedImageTaskSelection(candidates, func(total int) int { return total - 1 }).Channel.Id)
}

func TestCreateImageTask_RejectsSizeAs422(t *testing.T) {
	setupCreateTest(t)
	_, _, _, err := CreateImageTask(context.Background(), ImageTaskCreateInput{
		RawBody:     []byte(`{"model":"dall-e-3","prompt":"a cat","size":"1024x1024"}`),
		Operation:   ImageOperationGeneration,
		OwnerUserID: 1, IdempotencyKey: "k",
	})
	reqErr := dto.AsImageTaskRequestError(err)
	require.NotNil(t, reqErr)
	assert.Equal(t, 422, reqErr.StatusCode)
	assert.Equal(t, dto.ImageTaskErrUnsupportedParameter, reqErr.Code)
}

func TestCreateImageTask_NoCandidateIs503(t *testing.T) {
	setupCreateTest(t)
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"dall-e-3":0.04}`))
	// No channel/ability seeded → no candidate → fail closed.
	_, _, _, err := CreateImageTask(context.Background(), ImageTaskCreateInput{
		RawBody:        []byte(`{"model":"dall-e-3","prompt":"a cat"}`),
		Operation:      ImageOperationGeneration,
		OwnerUserID:    1,
		IdempotencyKey: "k",
		UsingGroup:     "default",
		UserBaseGroup:  "default",
	})
	reqErr := dto.AsImageTaskRequestError(err)
	require.NotNil(t, reqErr)
	assert.Equal(t, 503, reqErr.StatusCode)
}

func TestCreateImageTask_MissingRevisionSkipsCandidate(t *testing.T) {
	setupCreateTest(t)
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"dall-e-3":0.04}`))
	// Channel + ability but NO revision: the candidate is skipped (consistency
	// violation) and, with no other candidate, the create fails closed 503.
	cfg := `{"defaults":{"generation":"sync"}}`
	require.NoError(t, model.DB.Create(&model.Channel{
		Id: 7101, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled,
		Group: "default", Models: "dall-e-3", Key: "sk", ImageExecutionConfig: &cfg,
	}).Error)
	require.NoError(t, model.DB.Create(&model.Ability{Group: "default", Model: "dall-e-3", ChannelId: 7101, Enabled: true}).Error)

	_, _, _, err := CreateImageTask(context.Background(), ImageTaskCreateInput{
		RawBody: []byte(`{"model":"dall-e-3","prompt":"a cat"}`), Operation: ImageOperationGeneration,
		OwnerUserID: 1, IdempotencyKey: "k", UsingGroup: "default", UserBaseGroup: "default",
	})
	reqErr := dto.AsImageTaskRequestError(err)
	require.NotNil(t, reqErr)
	assert.Equal(t, 503, reqErr.StatusCode)
}

func TestCreateImageTask_MismatchedAdapterRevisionSkipsCandidate(t *testing.T) {
	setupCreateTest(t)
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"dall-e-3":0.04}`))
	cfg := `{"defaults":{"generation":"sync"}}`
	seedImageChannelForCreate(t, 7151, "default", "dall-e-3", cfg)
	require.NoError(t, model.DB.Where("channel_id = ?", 7151).Delete(&model.ChannelRevision{}).Error)
	require.NoError(t, model.DB.Create(&model.ChannelRevision{
		ChannelID: 7151, RevisionNumber: 2, Endpoint: "https://api.openai.com",
		CredentialRef: "channel:7151", AdapterVersion: "legacy-image-adapter/v0",
		Settings: mustImageRevisionSettings(t, cfg),
	}).Error)
	_, _, _, err := CreateImageTask(context.Background(), ImageTaskCreateInput{
		RawBody: []byte(`{"model":"dall-e-3","prompt":"a cat"}`), Operation: ImageOperationGeneration,
		OwnerUserID: 1, IdempotencyKey: "adapter-mismatch", UsingGroup: "default", UserBaseGroup: "default",
	})
	reqErr := dto.AsImageTaskRequestError(err)
	require.NotNil(t, reqErr)
	assert.Equal(t, 503, reqErr.StatusCode)
}

func TestCreateImageTask_UnsupportedConfigSkipsCandidate(t *testing.T) {
	setupCreateTest(t)
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"dall-e-3":0.04}`))
	// An unparseable execution mode makes ParseImageChannelExecutionConfig fail
	// inside trySelect; the candidate is skipped → 503.
	cfg := `{"defaults":{"generation":"weird-mode"}}`
	require.NoError(t, model.DB.Create(&model.Channel{
		Id: 7201, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled,
		Group: "default", Models: "dall-e-3", Key: "sk", ImageExecutionConfig: &cfg,
	}).Error)
	require.NoError(t, model.DB.Create(&model.Ability{Group: "default", Model: "dall-e-3", ChannelId: 7201, Enabled: true}).Error)
	require.NoError(t, model.DB.Create(&model.ChannelRevision{ChannelID: 7201, RevisionNumber: 1, Endpoint: "https://api.openai.com", CredentialRef: "channel:7201", AdapterVersion: "openai-image-adapter/v1", Settings: mustImageRevisionSettings(t, cfg)}).Error)

	_, _, _, err := CreateImageTask(context.Background(), ImageTaskCreateInput{
		RawBody: []byte(`{"model":"dall-e-3","prompt":"a cat"}`), Operation: ImageOperationGeneration,
		OwnerUserID: 1, IdempotencyKey: "k", UsingGroup: "default", UserBaseGroup: "default",
	})
	reqErr := dto.AsImageTaskRequestError(err)
	require.NotNil(t, reqErr)
	assert.Equal(t, 503, reqErr.StatusCode)
}

func TestCreateImageTask_ReservesAndProjectsAccepted(t *testing.T) {
	setupCreateTest(t)
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"dall-e-3":0.04}`))
	seedUser(t, 5001, 1000000)
	seedTokenForImage(t, 6001, 5001, "sk-tok", 1000000)
	seedImageChannelForCreate(t, 7001, "default", "dall-e-3", `{"defaults":{"generation":"sync","edit":"sync"}}`)

	in := ImageTaskCreateInput{
		RawBody:         []byte(`{"model":"dall-e-3","prompt":"a cat"}`),
		Operation:       ImageOperationGeneration,
		OwnerUserID:     5001,
		CreationTokenID: 6001,
		IdempotencyKey:  "k-1",
		UsingGroup:      "default",
		UserBaseGroup:   "default",
	}
	obj, replayed, _, err := CreateImageTask(context.Background(), in)
	require.NoError(t, err)
	require.NotNil(t, obj)
	assert.Equal(t, "image.task", obj.Object)
	assert.Equal(t, dto.ImageTaskStatusQueued, obj.Status)
	assert.NotEmpty(t, obj.ID)
	assert.False(t, replayed)
	var storedExecution model.ImageTaskExecution
	require.NoError(t, model.DB.Where("public_task_id = ?", obj.ID).First(&storedExecution).Error)
	var storedTask model.Task
	require.NoError(t, model.DB.First(&storedTask, storedExecution.TaskDBID).Error)
	assert.JSONEq(t, string(in.RawBody), string(storedTask.Data))
	assert.Equal(t, 7001, storedTask.ChannelId)

	// Same idempotency key + body → replay: same public id, replayed true.
	obj2, replayed2, _, err := CreateImageTask(context.Background(), in)
	require.NoError(t, err)
	assert.True(t, replayed2)
	assert.Equal(t, obj.ID, obj2.ID)
}

func TestCreateImageTask_TokenModelLimitFailsClosed(t *testing.T) {
	setupCreateTest(t)
	_, _, _, err := CreateImageTask(context.Background(), ImageTaskCreateInput{
		RawBody: []byte(`{"model":"dall-e-3","prompt":"a cat"}`), Operation: ImageOperationGeneration,
		OwnerUserID: 5101, IdempotencyKey: "scope-denied", UsingGroup: "default", UserBaseGroup: "default",
		TokenModelLimitEnabled: true, TokenModelLimit: map[string]bool{"gpt-image-1": true},
	})
	reqErr := dto.AsImageTaskRequestError(err)
	require.NotNil(t, reqErr)
	assert.Equal(t, 403, reqErr.StatusCode)
	assert.Equal(t, dto.ImageTaskErrUnauthorized, reqErr.Code)
}

func TestCreateImageTask_ReplaySurvivesChannelAndPriceChanges(t *testing.T) {
	setupCreateTest(t)
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"dall-e-3":0.04}`))
	seedUser(t, 5102, 1000000)
	seedTokenForImage(t, 6102, 5102, "sk-replay", 1000000)
	seedImageChannelForCreate(t, 7102, "default", "dall-e-3", `{"defaults":{"generation":"sync"}}`)
	in := ImageTaskCreateInput{
		RawBody: []byte(`{"model":"dall-e-3","prompt":"a cat"}`), Operation: ImageOperationGeneration,
		OwnerUserID: 5102, CreationTokenID: 6102, IdempotencyKey: "replay-after-change",
		UsingGroup: "default", UserBaseGroup: "default",
	}
	first, replayed, _, err := CreateImageTask(context.Background(), in)
	require.NoError(t, err)
	assert.False(t, replayed)
	require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", 7102).Update("status", common.ChannelStatusManuallyDisabled).Error)
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{}`))
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{}`))
	second, replayed, _, err := CreateImageTask(context.Background(), in)
	require.NoError(t, err)
	assert.True(t, replayed)
	assert.Equal(t, first.ID, second.ID)
}

func TestCreateImageTask_WalletInsufficientIs402(t *testing.T) {
	setupCreateTest(t)
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"dall-e-3":0.04}`))
	seedUser(t, 5002, 0) // no wallet quota
	seedTokenForImage(t, 6002, 5002, "sk-tok2", 1000000)
	seedImageChannelForCreate(t, 7002, "default", "dall-e-3", `{"defaults":{"generation":"sync"}}`)

	_, _, _, err := CreateImageTask(context.Background(), ImageTaskCreateInput{
		RawBody: []byte(`{"model":"dall-e-3","prompt":"a cat"}`), Operation: ImageOperationGeneration,
		OwnerUserID: 5002, CreationTokenID: 6002, IdempotencyKey: "k-2",
		UsingGroup: "default", UserBaseGroup: "default",
	})
	reqErr := dto.AsImageTaskRequestError(err)
	require.NotNil(t, reqErr)
	assert.Equal(t, 402, reqErr.StatusCode)
	assert.Equal(t, dto.ImageTaskErrInsufficientQuota, reqErr.Code)
}

func TestCreateImageTask_SubscriptionOnlyDoesNotFallBackToWallet(t *testing.T) {
	setupCreateTest(t)
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"dall-e-3":0.04}`))
	seedUser(t, 5003, 1000000)
	var user model.User
	require.NoError(t, model.DB.First(&user, 5003).Error)
	setting := user.GetSetting()
	setting.BillingPreference = "subscription_only"
	user.SetSetting(setting)
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", 5003).Update("setting", user.Setting).Error)
	seedTokenForImage(t, 6003, 5003, "sk-tok3", 1000000)
	seedImageChannelForCreate(t, 7003, "default", "dall-e-3", `{"defaults":{"generation":"sync"}}`)

	_, _, _, err := CreateImageTask(context.Background(), ImageTaskCreateInput{
		RawBody: []byte(`{"model":"dall-e-3","prompt":"a cat"}`), Operation: ImageOperationGeneration,
		OwnerUserID: 5003, CreationTokenID: 6003, IdempotencyKey: "subscription-only",
		UsingGroup: "default", UserBaseGroup: "default",
	})
	reqErr := dto.AsImageTaskRequestError(err)
	require.NotNil(t, reqErr)
	assert.Equal(t, 402, reqErr.StatusCode)
	var after model.User
	require.NoError(t, model.DB.First(&after, 5003).Error)
	assert.Equal(t, 1000000, after.Quota)
}

func TestCreateImageTask_TokenInsufficientRollsBackAggregate(t *testing.T) {
	setupCreateTest(t)
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"dall-e-3":0.04}`))
	seedUser(t, 5004, 1000000)
	seedTokenForImage(t, 6004, 5004, "sk-low", 1)
	seedImageChannelForCreate(t, 7004, "default", "dall-e-3", `{"defaults":{"generation":"sync"}}`)
	_, _, _, err := CreateImageTask(context.Background(), ImageTaskCreateInput{
		RawBody: []byte(`{"model":"dall-e-3","prompt":"a cat"}`), Operation: ImageOperationGeneration,
		OwnerUserID: 5004, CreationTokenID: 6004, IdempotencyKey: "token-low",
		UsingGroup: "default", UserBaseGroup: "default",
	})
	reqErr := dto.AsImageTaskRequestError(err)
	require.NotNil(t, reqErr)
	assert.Equal(t, 401, reqErr.StatusCode)
	var taskCount, ledgerCount int64
	require.NoError(t, model.DB.Model(&model.Task{}).Where("user_id = ?", 5004).Count(&taskCount).Error)
	require.NoError(t, model.DB.Model(&model.TaskBillingLedger{}).Count(&ledgerCount).Error)
	assert.Zero(t, taskCount)
	assert.Zero(t, ledgerCount)
	var after model.User
	require.NoError(t, model.DB.First(&after, 5004).Error)
	assert.Equal(t, 1000000, after.Quota)
}

func TestCreateImageTask_InFlightCapRejectsWithoutBilling(t *testing.T) {
	setupCreateTest(t)
	constant.MaxImageTasksPerUser = 1
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"dall-e-3":0.04}`))
	seedUser(t, 5005, 1000000)
	seedTokenForImage(t, 6005, 5005, "sk-cap", 1000000)
	seedImageChannelForCreate(t, 7005, "default", "dall-e-3", `{"defaults":{"generation":"sync"}}`)
	require.NoError(t, model.DB.Create(&model.ImageTaskExecution{
		PublicTaskID: "imgtask_existing", TaskDBID: 99999, OwnerUserID: 5005,
		Operation: model.ImageTaskOperationGeneration, IdempotencyKey: "existing", RequestHash: "h",
		State: model.ImageTaskStateQueued,
	}).Error)
	_, _, _, err := CreateImageTask(context.Background(), ImageTaskCreateInput{
		RawBody: []byte(`{"model":"dall-e-3","prompt":"a cat"}`), Operation: ImageOperationGeneration,
		OwnerUserID: 5005, CreationTokenID: 6005, IdempotencyKey: "cap-new",
		UsingGroup: "default", UserBaseGroup: "default",
	})
	reqErr := dto.AsImageTaskRequestError(err)
	require.NotNil(t, reqErr)
	assert.Equal(t, 429, reqErr.StatusCode)
	var after model.User
	require.NoError(t, model.DB.First(&after, 5005).Error)
	assert.Equal(t, 1000000, after.Quota)
}
