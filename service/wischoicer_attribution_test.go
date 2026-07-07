package service

import (
	"context"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRefundTaskQuota_ReusesWischoicerSnapshot 验证 refund 阶段从 task.PrivateData
// 快照复用归因（billing_stage=refund）与原始提交链路 ID（WIS-499 回炉收敛 b9996b28）。
func TestRefundTaskQuota_ReusesWischoicerSnapshot(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 50, 50, 50
	const initQuota, preConsumed = 10000, 3000
	seedUser(t, userID, initQuota)
	seedToken(t, tokenID, userID, "sk-wis-key", 5000)
	seedChannel(t, channelID)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
	task.PrivateData.Wischoicer = &model.WischoicerAttribution{
		SchemaVersion:    1,
		SourceService:    model.WischoicerSourceServiceContentWorkstation,
		InternalFunction: true,
		FeatureCode:      model.FeatureCodeMerchVideoClone,
		OperationCode:    "merch_video_clone.step3.manual_segment.submit",
		BizTaskId:        "biz-task-1",
		AccountId:        "u1",
		AppUserId:        "u1",
	}
	task.PrivateData.SubmitRequestId = "rid-submit-001"
	task.PrivateData.SubmitUpstreamRequestId = "urid-submit-001"

	RefundTaskQuota(ctx, task, "task failed: upstream error")

	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeRefund, log.Type)

	other, err := common.StrToMap(log.Other)
	require.NoError(t, err)

	wMap, ok := other["wischoicer"].(map[string]interface{})
	require.True(t, ok, "refund 日志应携带 other.wischoicer")
	assert.Equal(t, model.BillingStageRefund, wMap["billing_stage"])
	assert.Equal(t, model.FeatureCodeMerchVideoClone, wMap["feature_code"])
	assert.Equal(t, "biz-task-1", wMap["biz_task_id"])

	// 链路 ID 复用 submit 快照
	assert.Equal(t, "rid-submit-001", other["request_id"])
	assert.Equal(t, "urid-submit-001", other["upstream_request_id"])
}

// TestRecalculateTaskQuota_PositiveDelta_WischoicerSettle 验证正向差额（补扣）走
// billing_stage=settle 且复用 submit 快照。
func TestRecalculateTaskQuota_PositiveDelta_WischoicerSettle(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 51, 51, 51
	const initQuota, preConsumed = 10000, 3000
	seedUser(t, userID, initQuota)
	seedToken(t, tokenID, userID, "sk-wis-key2", 5000)
	seedChannel(t, channelID)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
	task.PrivateData.Wischoicer = &model.WischoicerAttribution{
		SchemaVersion:    1,
		SourceService:    model.WischoicerSourceServiceContentWorkstation,
		InternalFunction: true,
		FeatureCode:      model.FeatureCodeMerchVideoClone,
		OperationCode:    "merch_video_clone.step3.manual_segment.submit",
		BizTaskId:        "biz-task-2",
		AccountId:        "u2",
		AppUserId:        "u2",
	}
	task.PrivateData.SubmitRequestId = "rid-submit-002"
	task.PrivateData.SubmitUpstreamRequestId = "urid-submit-002"

	// actualQuota > preConsumed → 正向差额补扣（settle）
	RecalculateTaskQuota(ctx, task, 5000, "token重算")

	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeConsume, log.Type)
	assert.Equal(t, 2000, log.Quota, "差额 = actual - preConsumed")

	other, err := common.StrToMap(log.Other)
	require.NoError(t, err)
	wMap, ok := other["wischoicer"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, model.BillingStageSettle, wMap["billing_stage"])
	assert.Equal(t, "rid-submit-002", other["request_id"])
	assert.Equal(t, "urid-submit-002", other["upstream_request_id"])
}

// TestRecalculateTaskQuota_NegativeDelta_WischoicerRefund 验证负向差额（退回）走
// billing_stage=refund。
func TestRecalculateTaskQuota_NegativeDelta_WischoicerRefund(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 52, 52, 52
	const initQuota, preConsumed = 10000, 3000
	seedUser(t, userID, initQuota)
	seedToken(t, tokenID, userID, "sk-wis-key3", 5000)
	seedChannel(t, channelID)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
	task.PrivateData.Wischoicer = &model.WischoicerAttribution{
		SchemaVersion:    1,
		SourceService:    model.WischoicerSourceServiceContentWorkstation,
		InternalFunction: true,
		FeatureCode:      model.FeatureCodeImageCreation,
		OperationCode:    "image_creation.generate",
		BizTaskId:        "biz-task-3",
		AccountId:        "u3",
		AppUserId:        "u3",
	}
	task.PrivateData.SubmitRequestId = "rid-submit-003"

	// actualQuota < preConsumed → 负向差额退回（refund）
	RecalculateTaskQuota(ctx, task, 1000, "adaptor调整")

	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeRefund, log.Type)
	other, err := common.StrToMap(log.Other)
	require.NoError(t, err)
	wMap, ok := other["wischoicer"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, model.BillingStageRefund, wMap["billing_stage"])
}

// TestTaskBilling_NoWischoicerSnapshot_NoAttribution 验证无归因快照的任务不写
// other.wischoicer（不污染普通异步任务日志）。
func TestTaskBilling_NoWischoicerSnapshot_NoAttribution(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 53, 53, 53
	seedUser(t, userID, 10000)
	seedToken(t, tokenID, userID, "sk-plain", 5000)
	seedChannel(t, channelID)

	task := makeTask(userID, channelID, 3000, tokenID, BillingSourceWallet, 0)
	// 故意不设 Wischoicer 快照
	RefundTaskQuota(ctx, task, "plain task failed")

	log := getLastLog(t)
	require.NotNil(t, log)
	other, err := common.StrToMap(log.Other)
	require.NoError(t, err)
	_, hasWisc := other["wischoicer"]
	assert.False(t, hasWisc, "无归因快照时不应写 other.wischoicer")
	_, hasRid := other["request_id"]
	assert.False(t, hasRid, "无归因快照时不应写 other.request_id")
}
