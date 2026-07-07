package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 构造一条日志的 other JSON。attributed=true 时带 wischoicer 归因对象。
func buildOtherJSON(featureCode, opCode, bizTaskID, billingStage, taskID, snapReq, snapUp string) string {
	m := map[string]interface{}{}
	if taskID != "" {
		m["task_id"] = taskID
	}
	w := map[string]interface{}{
		"schema_version":    1,
		"source_service":    "content-workstation",
		"internal_function": true,
		"feature_code":      featureCode,
		"operation_code":    opCode,
		"biz_task_id":       bizTaskID,
		"account_id":        "acct-1",
		"app_user_id":       "acct-1",
	}
	if billingStage != "" {
		w["billing_stage"] = billingStage
	}
	if snapReq != "" {
		w["request_id"] = snapReq
	}
	if snapUp != "" {
		w["upstream_request_id"] = snapUp
	}
	m["wischoicer"] = w
	return common.MapToJsonStr(m)
}

// seedFeatureUsageLogs 灌入一整套归因 + 普通日志，覆盖验收场景。
//
//	userId（归因用户）：
//	  merch_video_clone / biz-1：submit(100) + settle(50) + refund(30) —— 同一业务任务三阶段
//	  image_creation：request(200)
//	  uncategorized（feature_code 缺失）：request(40)
//	同一 userId 的普通日志（无归因）：request(9999) —— 必须不被任何聚合误伤
func seedFeatureUsageLogs(t *testing.T, userId int, baseTs int64) {
	t.Helper()
	logs := []*Log{
		// merch_video_clone 同一 biz_task_id 的 submit / settle / refund
		{UserId: userId, CreatedAt: baseTs + 10, Type: LogTypeConsume, ModelName: "doubao-seedance", TokenId: 1, Quota: 100,
			RequestId: "req-submit", UpstreamRequestId: "up-submit",
			Other: buildOtherJSON("merch_video_clone", "merch_video_clone.step3.full_video.segment_submit", "biz-1", common.WischoicerStageSubmit, "task-1", "", "")},
		{UserId: userId, CreatedAt: baseTs + 20, Type: LogTypeConsume, ModelName: "doubao-seedance", TokenId: 1, Quota: 50,
			RequestId: "req-settle", UpstreamRequestId: "up-settle",
			Other: buildOtherJSON("merch_video_clone", "merch_video_clone.step3.full_video.segment_submit", "biz-1", common.WischoicerStageSettle, "task-1", "req-submit", "up-submit")},
		{UserId: userId, CreatedAt: baseTs + 30, Type: LogTypeRefund, ModelName: "doubao-seedance", TokenId: 1, Quota: 30,
			Other: buildOtherJSON("merch_video_clone", "merch_video_clone.step3.full_video.segment_submit", "biz-1", common.WischoicerStageRefund, "task-1", "req-submit", "up-submit")},
		// image_creation 同步消费
		{UserId: userId, CreatedAt: baseTs + 40, Type: LogTypeConsume, ModelName: "gpt-image-2", TokenId: 1, Quota: 200,
			RequestId: "req-img", UpstreamRequestId: "up-img",
			Other: buildOtherJSON("image_creation", "image_creation.generate", "biz-2", common.WischoicerStageRequest, "", "", "")},
		// uncategorized：归因有效但 feature_code 缺失
		{UserId: userId, CreatedAt: baseTs + 50, Type: LogTypeConsume, ModelName: "gemini-3.5-flash", TokenId: 1, Quota: 40,
			RequestId: "req-uncat",
			Other:     buildOtherJSON("", "some.unknown.op", "biz-3", common.WischoicerStageRequest, "", "", "")},
		// 普通 API Key 消耗：无 wischoicer，绝不能被聚合误伤
		{UserId: userId, CreatedAt: baseTs + 60, Type: LogTypeConsume, ModelName: "gpt-4o", TokenId: 2, Quota: 9999,
			RequestId: "req-normal", Other: `{"is_task":false}`},
	}
	for _, l := range logs {
		// 绕过 ensureLogRequestId，保留显式 RequestId 以验证 settle/refund 复用语义。
		require.NoError(t, LOG_DB.Create(l).Error)
	}
}

func TestFeatureUsageSummary_AggregationAndUncategorizedNotHarmed(t *testing.T) {
	baseTs := int64(1_700_000_000)
	seedFeatureUsageLogs(t, 7101, baseTs)
	start, end := baseTs, baseTs+1000

	res, err := GetFeatureUsageSummary(7101, start, end, "")
	require.NoError(t, err)

	// totals = 全部归因日志净值：100(submit)+50(settle)-30(refund)+200(img)+40(uncat) = 360
	assert.Equal(t, 360, res.Totals.Quota)
	assert.InDelta(t, float64(360)/common.QuotaPerUnit, res.Totals.CostRMB, 1e-9)
	// request_count 只计 request/submit：submit + img + uncat = 3（settle/refund 不计）
	assert.Equal(t, 3, res.Totals.RequestCount)

	// 两个已知 feature
	require.Len(t, res.Features, 2)
	var merch, image FeatureUsageFeatureAgg
	for _, f := range res.Features {
		switch f.FeatureCode {
		case "merch_video_clone":
			merch = f
		case "image_creation":
			image = f
		}
	}
	// merch: net 100+50-30=120；request_count 只计 submit=1；task_count=1
	assert.Equal(t, 120, merch.Quota)
	assert.Equal(t, 1, merch.RequestCount)
	assert.Equal(t, 1, merch.TaskCount)
	assert.Equal(t, "爆款复刻中心 - 带货视频爆款复刻", merch.FeatureName)
	// image: 200, request_count=1
	assert.Equal(t, 200, image.Quota)
	assert.Equal(t, 1, image.RequestCount)

	// uncategorized：present=true, quota=40, request_count=1
	assert.True(t, res.Uncategorized.Present)
	assert.Equal(t, 40, res.Uncategorized.Quota)
	assert.Equal(t, 1, res.Uncategorized.RequestCount)
}

func TestFeatureUsageSummary_FeatureFilterRejectsUncategorized(t *testing.T) {
	baseTs := int64(1_700_000_000)
	seedFeatureUsageLogs(t, 7102, baseTs)
	res, err := GetFeatureUsageSummary(7102, baseTs, baseTs+1000, "image_creation")
	require.NoError(t, err)
	require.Len(t, res.Features, 1)
	assert.Equal(t, "image_creation", res.Features[0].FeatureCode)
	assert.Equal(t, 200, res.Totals.Quota)
	// 指定 feature 后 uncategorized 不适用
	assert.False(t, res.Uncategorized.Present)
}

func TestFeatureUsageTasks_AggregateByFeatureAndBizTaskID(t *testing.T) {
	baseTs := int64(1_700_000_100)
	seedFeatureUsageLogs(t, 7103, baseTs)
	res, err := GetFeatureUsageTasks(7103, baseTs, baseTs+1000, FeatureUsageTasksFilter{}, 1, 20)
	require.NoError(t, err)
	// 任务聚合键 = feature_code + biz_task_id：merch/biz-1、image/biz-2（uncategorized 不支持）
	assert.Equal(t, 2, res.Total)
	// merch/biz-1：submit+settle+refund 净值 120；request_count 只计 submit=1
	var merchTask FeatureUsageTaskAgg
	for _, it := range res.Items {
		if it.BizTaskID == "biz-1" {
			merchTask = it
		}
	}
	assert.Equal(t, "merch_video_clone", merchTask.FeatureCode)
	assert.Equal(t, 120, merchTask.Quota)
	assert.Equal(t, 1, merchTask.RequestCount)
}

func TestFeatureUsageDetails_SettleRefundReusesSnapshotRequestID(t *testing.T) {
	baseTs := int64(1_700_000_200)
	seedFeatureUsageLogs(t, 7104, baseTs)
	res, err := GetFeatureUsageDetails(7104, baseTs, baseTs+1000, FeatureUsageDetailsFilter{}, 1, 100)
	require.NoError(t, err)
	// 5 条归因明细（普通日志不出现）
	assert.Equal(t, 5, res.Total)

	// submit：request_id 取顶层 req-submit
	submit := findDetail(res.Items, common.WischoicerStageSubmit)
	require.NotNil(t, submit)
	require.NotNil(t, submit.RequestID)
	assert.Equal(t, "req-submit", *submit.RequestID)
	require.NotNil(t, submit.UpstreamRequestID)
	assert.Equal(t, "up-submit", *submit.UpstreamRequestID)
	// provider_task_id 来源于异步任务日志的 task_id
	require.NotNil(t, submit.ProviderTaskID)
	assert.Equal(t, "task-1", *submit.ProviderTaskID)

	// settle：request_id 复用 submit 快照（req-submit），而不是顶层 req-settle
	settle := findDetail(res.Items, common.WischoicerStageSettle)
	require.NotNil(t, settle)
	require.NotNil(t, settle.RequestID)
	assert.Equal(t, "req-submit", *settle.RequestID, "settle must reuse submit snapshot request_id")
	assert.Equal(t, "up-submit", *settle.UpstreamRequestID)
	// settle quota 正号
	assert.Equal(t, 50, settle.Quota)
	assert.Equal(t, "consume", settle.LogType)

	// refund：复用快照；quota 负号；log_type=refund
	refund := findDetail(res.Items, common.WischoicerStageRefund)
	require.NotNil(t, refund)
	assert.Equal(t, "req-submit", *refund.RequestID)
	assert.Equal(t, -30, refund.Quota, "refund quota must be negative (net)")
	assert.Equal(t, "refund", refund.LogType)
}

func TestFeatureUsageDetails_NoSnapshotReturnsNullRequestID(t *testing.T) {
	// 历史/补数 refund 日志：归因有效但无 submit 快照（wischoicer 内无 request_id）。
	baseTs := int64(1_700_000_300)
	require.NoError(t, LOG_DB.Create(&Log{
		UserId: 7002, CreatedAt: baseTs + 5, Type: LogTypeRefund, ModelName: "doubao-seedance", Quota: 30,
		Other: buildOtherJSON("merch_video_clone", "merch_video_clone.step3.full_video.segment_submit", "biz-x", common.WischoicerStageRefund, "task-x", "", ""),
	}).Error)
	res, err := GetFeatureUsageDetails(7002, baseTs, baseTs+1000, FeatureUsageDetailsFilter{}, 1, 100)
	require.NoError(t, err)
	require.Equal(t, 1, res.Total)
	require.Len(t, res.Items, 1)
	// 无快照 → request_id / upstream_request_id 为 null
	assert.Nil(t, res.Items[0].RequestID)
	assert.Nil(t, res.Items[0].UpstreamRequestID)
}

func TestFeatureUsageDetails_UncategorizedFilter(t *testing.T) {
	baseTs := int64(1_700_000_400)
	seedFeatureUsageLogs(t, 7105, baseTs)
	res, err := GetFeatureUsageDetails(7105, baseTs, baseTs+1000, FeatureUsageDetailsFilter{FeatureCode: "uncategorized"}, 1, 100)
	require.NoError(t, err)
	// 只剩 uncategorized 那一条（feature_code 缺失）
	assert.Equal(t, 1, res.Total)
	assert.Equal(t, "", res.Items[0].FeatureCode)
}

// findDetail 按 billing_stage 找明细。
func findDetail(items []FeatureUsageDetailItem, stage string) *FeatureUsageDetailItem {
	for i := range items {
		if items[i].BillingStage == stage {
			return &items[i]
		}
	}
	return nil
}
