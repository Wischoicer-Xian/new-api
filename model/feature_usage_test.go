package model

import (
	"encoding/json"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 构造一条日志的 other JSON。attributed=true 时带 wischoicer 归因对象。
// providerTaskID 写 other.provider_task_id（上游真实任务 ID，仅异步任务日志有）。
// bizTaskTitle 写归因 biz_task_title（WIS-514 方案 A task_keyword 过滤测试依赖）。
func buildOtherJSON(featureCode, opCode, bizTaskID, bizTaskTitle, billingStage, providerTaskID, snapReq, snapUp string) string {
	m := map[string]interface{}{}
	if providerTaskID != "" {
		m["provider_task_id"] = providerTaskID
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
	if bizTaskTitle != "" {
		w["biz_task_title"] = bizTaskTitle
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
			Other: buildOtherJSON("merch_video_clone", "merch_video_clone.step3.full_video.segment_submit", "biz-1", "爆款复刻带货视频任务A", common.WischoicerStageSubmit, "upstream-task-1", "", "")},
		{UserId: userId, CreatedAt: baseTs + 20, Type: LogTypeConsume, ModelName: "doubao-seedance", TokenId: 1, Quota: 50,
			RequestId: "req-settle", UpstreamRequestId: "up-settle",
			Other: buildOtherJSON("merch_video_clone", "merch_video_clone.step3.full_video.segment_submit", "biz-1", "爆款复刻带货视频任务A", common.WischoicerStageSettle, "upstream-task-1", "req-submit", "up-submit")},
		{UserId: userId, CreatedAt: baseTs + 30, Type: LogTypeRefund, ModelName: "doubao-seedance", TokenId: 1, Quota: 30,
			Other: buildOtherJSON("merch_video_clone", "merch_video_clone.step3.full_video.segment_submit", "biz-1", "爆款复刻带货视频任务A", common.WischoicerStageRefund, "upstream-task-1", "req-submit", "up-submit")},
		// image_creation 同步消费
		{UserId: userId, CreatedAt: baseTs + 40, Type: LogTypeConsume, ModelName: "gpt-image-2", TokenId: 1, Quota: 200,
			RequestId: "req-img", UpstreamRequestId: "up-img",
			Other: buildOtherJSON("image_creation", "image_creation.generate", "biz-2", "图片生成任务B", common.WischoicerStageRequest, "", "", "")},
		// uncategorized：归因有效但 feature_code 缺失
		{UserId: userId, CreatedAt: baseTs + 50, Type: LogTypeConsume, ModelName: "gemini-3.5-flash", TokenId: 1, Quota: 40,
			RequestId: "req-uncat",
			Other:     buildOtherJSON("", "some.unknown.op", "biz-3", "未归因历史任务C", common.WischoicerStageRequest, "", "", "")},
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

	res, err := GetFeatureUsageSummary(7101, start, end, FeatureUsageSummaryFilter{})
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
	assert.Equal(t, "爆款复刻中心 - 视频爆款复刻", merch.FeatureName)
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
	res, err := GetFeatureUsageSummary(7102, baseTs, baseTs+1000, FeatureUsageSummaryFilter{FeatureCode: "image_creation"})
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
	// provider_task_id = 上游真实任务 ID（非 new-api task_xxx）
	require.NotNil(t, submit.ProviderTaskID)
	assert.Equal(t, "upstream-task-1", *submit.ProviderTaskID)

	// settle：request_id 复用 submit 快照（req-submit），而不是顶层 req-settle
	settle := findDetail(res.Items, common.WischoicerStageSettle)
	require.NotNil(t, settle)
	require.NotNil(t, settle.RequestID)
	assert.Equal(t, "req-submit", *settle.RequestID, "settle must reuse submit snapshot request_id")
	assert.Equal(t, "up-submit", *settle.UpstreamRequestID)
	// settle 的 provider_task_id 复用 submit 冻结的上游 ID
	require.NotNil(t, settle.ProviderTaskID)
	assert.Equal(t, "upstream-task-1", *settle.ProviderTaskID)
	// settle quota 正号
	assert.Equal(t, 50, settle.Quota)
	assert.Equal(t, "consume", settle.LogType)

	// refund：复用快照；quota 负号；log_type=refund
	refund := findDetail(res.Items, common.WischoicerStageRefund)
	require.NotNil(t, refund)
	assert.Equal(t, "req-submit", *refund.RequestID)
	require.NotNil(t, refund.ProviderTaskID)
	assert.Equal(t, "upstream-task-1", *refund.ProviderTaskID)
	assert.Equal(t, -30, refund.Quota, "refund quota must be negative (net)")
	assert.Equal(t, "refund", refund.LogType)

	// request 阶段（image 同步消费）无异步任务 → provider_task_id 为 null
	var reqImg *FeatureUsageDetailItem
	for i := range res.Items {
		if res.Items[i].FeatureCode == "image_creation" {
			reqImg = &res.Items[i]
			break
		}
	}
	require.NotNil(t, reqImg)
	assert.Nil(t, reqImg.ProviderTaskID, "request stage has no async task → null")
}

func TestFeatureUsageDetails_NoSnapshotReturnsNullRequestID(t *testing.T) {
	// 历史/补数 refund 日志：归因有效但无 submit 快照（wischoicer 内无 request_id）。
	baseTs := int64(1_700_000_300)
	require.NoError(t, LOG_DB.Create(&Log{
		UserId: 7002, CreatedAt: baseTs + 5, Type: LogTypeRefund, ModelName: "doubao-seedance", Quota: 30,
		Other: buildOtherJSON("merch_video_clone", "merch_video_clone.step3.full_video.segment_submit", "biz-x", "", common.WischoicerStageRefund, "upstream-x", "", ""),
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

// TestFeatureUsageDetails_ResponseOmitsModelAndRequestIDs WIS-514：客户侧 self details
// 响应序列化层不得出现 model_name / request_id / upstream_request_id。
// 归因闭环（settle/refund 复用 submit 快照的 request_id）由上面两条 *_RequestID 单测在
// 结构体层守护——这里只断言 JSON 线上不暴露给客户。
func TestFeatureUsageDetails_ResponseOmitsModelAndRequestIDs(t *testing.T) {
	baseTs := int64(1_700_000_500)
	seedFeatureUsageLogs(t, 7106, baseTs)
	res, err := GetFeatureUsageDetails(7106, baseTs, baseTs+1000, FeatureUsageDetailsFilter{}, 1, 100)
	require.NoError(t, err)
	require.NotEmpty(t, res.Items)

	b, err := json.Marshal(res)
	require.NoError(t, err)

	var wire struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	require.NoError(t, json.Unmarshal(b, &wire))
	require.NotEmpty(t, wire.Items)
	for i, item := range wire.Items {
		_, hasModel := item["model_name"]
		_, hasReq := item["request_id"]
		_, hasUp := item["upstream_request_id"]
		assert.False(t, hasModel, "items[%d] 客户侧响应不得包含 model_name", i)
		assert.False(t, hasReq, "items[%d] 客户侧响应不得包含 request_id", i)
		assert.False(t, hasUp, "items[%d] 客户侧响应不得包含 upstream_request_id", i)
	}
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

// WIS-514 方案 A：summary 加性扩参 biz_task_id / task_keyword / operation_code，与 tasks/details 同口径，
// 使顶部汇总受任务/操作筛选影响。下方用同一 seed 验证 summary 三参过滤；向后兼容（零值 filter 行为不变）
// 由 TestFeatureUsageSummary_AggregationAndUncategorizedNotHarmed / _FeatureFilterRejectsUncategorized 守护
// （它们传零值 / 仅 FeatureCode 的 filter，结果与扩参前一致）。

func TestFeatureUsageSummary_FiltersByTaskKeyword(t *testing.T) {
	baseTs := int64(1_700_000_600)
	seedFeatureUsageLogs(t, 7107, baseTs)
	// task_keyword="带货视频" 只命中 merch/biz-1（title "爆款复刻带货视频任务A"）。
	res, err := GetFeatureUsageSummary(7107, baseTs, baseTs+1000, FeatureUsageSummaryFilter{TaskKeyword: "带货视频"})
	require.NoError(t, err)
	assert.Equal(t, 120, res.Totals.Quota) // 100(submit)+50(settle)-30(refund)
	assert.Equal(t, 1, res.Totals.RequestCount)
	require.Len(t, res.Features, 1)
	assert.Equal(t, "merch_video_clone", res.Features[0].FeatureCode)
	assert.Equal(t, 120, res.Features[0].Quota)
	assert.False(t, res.Uncategorized.Present, "task_keyword 限定到已知 feature，uncategorized 不应出现")
}

func TestFeatureUsageSummary_FiltersByBizTaskID(t *testing.T) {
	baseTs := int64(1_700_000_700)
	seedFeatureUsageLogs(t, 7108, baseTs)
	// biz_task_id="biz-2" 只命中 image_creation。
	res, err := GetFeatureUsageSummary(7108, baseTs, baseTs+1000, FeatureUsageSummaryFilter{BizTaskID: "biz-2"})
	require.NoError(t, err)
	assert.Equal(t, 200, res.Totals.Quota)
	assert.Equal(t, 1, res.Totals.RequestCount)
	require.Len(t, res.Features, 1)
	assert.Equal(t, "image_creation", res.Features[0].FeatureCode)
}

func TestFeatureUsageSummary_FiltersByOperationCode(t *testing.T) {
	baseTs := int64(1_700_000_800)
	seedFeatureUsageLogs(t, 7109, baseTs)
	// operation_code 只命中 image_creation.generate。
	res, err := GetFeatureUsageSummary(7109, baseTs, baseTs+1000, FeatureUsageSummaryFilter{OperationCode: "image_creation.generate"})
	require.NoError(t, err)
	assert.Equal(t, 200, res.Totals.Quota)
	require.Len(t, res.Features, 1)
	assert.Equal(t, "image_creation", res.Features[0].FeatureCode)
}

// WIS-514 方案 A：details 加 task_keyword，与 tasks 同口径，调用明细受任务标题关键字筛选。
func TestFeatureUsageDetails_FiltersByTaskKeyword(t *testing.T) {
	baseTs := int64(1_700_000_900)
	seedFeatureUsageLogs(t, 7110, baseTs)
	res, err := GetFeatureUsageDetails(7110, baseTs, baseTs+1000, FeatureUsageDetailsFilter{TaskKeyword: "图片"}, 1, 100)
	require.NoError(t, err)
	// task_keyword="图片" 只命中 image/biz-2 一条明细。
	require.Equal(t, 1, res.Total)
	require.Len(t, res.Items, 1)
	assert.Equal(t, "image_creation", res.Items[0].FeatureCode)
	assert.Equal(t, "biz-2", res.Items[0].BizTaskID)
}

// --- WIS-523 feature-usage analytics ---

// seedAnalyticsBucketLogs 灌入跨多个小时/自然日的归因日志，用于 hour/day/week 桶聚合验证。
// baseTs 取 UTC 整点整日（2023-11-14 00:00 UTC，周二），便于断言整数取模桶。
func seedAnalyticsBucketLogs(t *testing.T, userId int, baseTs int64) {
	t.Helper()
	logs := []*Log{
		// merch：同一天的两个不同小时桶
		{UserId: userId, CreatedAt: baseTs + 10, Type: LogTypeConsume, Quota: 100,
			Other: buildOtherJSON("merch_video_clone", "merch_video_clone.step3.full_video.segment_submit", "biz-1", "", common.WischoicerStageSubmit, "", "", "")},
		{UserId: userId, CreatedAt: baseTs + 3610, Type: LogTypeConsume, Quota: 50,
			Other: buildOtherJSON("merch_video_clone", "merch_video_clone.step3.full_video.segment_submit", "biz-1", "", common.WischoicerStageSubmit, "", "", "")},
		// image：当天 + 次日
		{UserId: userId, CreatedAt: baseTs + 20, Type: LogTypeConsume, Quota: 200,
			Other: buildOtherJSON("image_creation", "image_creation.generate", "biz-2", "", common.WischoicerStageRequest, "", "", "")},
		{UserId: userId, CreatedAt: baseTs + 86400 + 30, Type: LogTypeConsume, Quota: 80,
			Other: buildOtherJSON("image_creation", "image_creation.generate", "biz-2", "", common.WischoicerStageRequest, "", "", "")},
	}
	for _, l := range logs {
		require.NoError(t, LOG_DB.Create(l).Error)
	}
}

// TestFeatureUsageAnalytics_HourDayWeekAggregation 核心桶聚合：同种子分别按
// hour/day/week 聚合，验证桶起点、points 归并与排序、totals 与 features；并验证空粒度自动推断。
func TestFeatureUsageAnalytics_HourDayWeekAggregation(t *testing.T) {
	// 2023-11-14 00:00 UTC（周二），UTC 整点整日。
	baseTs := int64(1_699_920_000)
	seedAnalyticsBucketLogs(t, 7201, baseTs)
	// 窗口覆盖全部日志（含次日那条），86460s = 1441 整分钟。
	start, end := baseTs, baseTs+86400+60

	// --- hour ---
	hour, err := GetFeatureUsageAnalytics(7201, start, end, FeatureUsageGranularityHour, FeatureUsageAnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, FeatureUsageGranularityHour, hour.Granularity)
	// totals 与粒度无关：100+50+200+80=430；request_count 全计 = 4；token 未设 = 0。
	assert.Equal(t, 430, hour.Totals.Quota)
	assert.Equal(t, 4, hour.Totals.RequestCount)
	assert.Equal(t, 0, hour.Totals.TokenUsed)
	assert.InDelta(t, 0.0, hour.Totals.AvgTPM, 1e-9) // token=0 → tpm=0
	assert.InDelta(t, float64(4)/(86460.0/60.0), hour.Totals.AvgRPM, 1e-9)
	// hour 桶：h0(baseTs) 含 merch100+image200 → 2 点；h1(baseTs+3600) 含 merch50 → 1 点；次日 00:00 含 image80 → 1 点。
	require.Len(t, hour.Points, 4)
	// 同桶内按 quota 降序：h0 image(200) 在 merch(100) 前。
	assert.Equal(t, baseTs, hour.Points[0].BucketTs)
	assert.Equal(t, "image_creation", hour.Points[0].FeatureCode)
	assert.Equal(t, 200, hour.Points[0].Quota)
	assert.Equal(t, baseTs, hour.Points[1].BucketTs)
	assert.Equal(t, "merch_video_clone", hour.Points[1].FeatureCode)
	assert.Equal(t, 100, hour.Points[1].Quota)
	assert.Equal(t, baseTs+3600, hour.Points[2].BucketTs)
	assert.Equal(t, "merch_video_clone", hour.Points[2].FeatureCode)
	assert.Equal(t, 50, hour.Points[2].Quota)
	assert.Equal(t, baseTs+86400, hour.Points[3].BucketTs)
	assert.Equal(t, "image_creation", hour.Points[3].FeatureCode)
	assert.Equal(t, 80, hour.Points[3].Quota)
	// hour 桶标签格式 "MM-DD HH:mm"。
	assert.Equal(t, "11-14 00:00", hour.Points[0].BucketLabel)
	assert.Equal(t, "11-14 01:00", hour.Points[2].BucketLabel)

	// features 按 quota 降序：image(280) 在 merch(150) 前。
	require.Len(t, hour.Features, 2)
	assert.Equal(t, "image_creation", hour.Features[0].FeatureCode)
	assert.Equal(t, 280, hour.Features[0].Quota)
	assert.Equal(t, 2, hour.Features[0].RequestCount)
	assert.Equal(t, "merch_video_clone", hour.Features[1].FeatureCode)
	assert.Equal(t, 150, hour.Features[1].Quota)

	// --- day ---
	day, err := GetFeatureUsageAnalytics(7201, start, end, FeatureUsageGranularityDay, FeatureUsageAnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, FeatureUsageGranularityDay, day.Granularity)
	// day 桶：d0(baseTs) merch150+image200 → 2 点；d1(baseTs+86400) image80 → 1 点。
	require.Len(t, day.Points, 3)
	assert.Equal(t, baseTs, day.Points[0].BucketTs) // 同日内 image(200) 在前
	assert.Equal(t, "image_creation", day.Points[0].FeatureCode)
	assert.Equal(t, 200, day.Points[0].Quota)
	assert.Equal(t, baseTs, day.Points[1].BucketTs)
	assert.Equal(t, "merch_video_clone", day.Points[1].FeatureCode)
	assert.Equal(t, 150, day.Points[1].Quota)
	assert.Equal(t, baseTs+86400, day.Points[2].BucketTs)
	assert.Equal(t, "image_creation", day.Points[2].FeatureCode)
	assert.Equal(t, 80, day.Points[2].Quota)
	assert.Equal(t, "11-14", day.Points[0].BucketLabel) // day 标签 "MM-DD"

	// --- week ---（2023-11-14 周二 → 周一桶 = baseTs-86400）
	week, err := GetFeatureUsageAnalytics(7201, start, end, FeatureUsageGranularityWeek, FeatureUsageAnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, FeatureUsageGranularityWeek, week.Granularity)
	weekBucket := baseTs - 86400 // 2023-11-13 00:00 UTC（周一）
	// 全部日志落在同一周桶，按 feature 拆 2 点。
	require.Len(t, week.Points, 2)
	assert.Equal(t, weekBucket, week.Points[0].BucketTs)
	assert.Equal(t, weekBucket, week.Points[1].BucketTs)
	assert.Equal(t, "image_creation", week.Points[0].FeatureCode)
	assert.Equal(t, 280, week.Points[0].Quota) // 200+80
	assert.Equal(t, "merch_video_clone", week.Points[1].FeatureCode)
	assert.Equal(t, 150, week.Points[1].Quota)

	// --- 自动推断（空粒度）---
	h, err := GetFeatureUsageAnalytics(7201, start, start+1000, "", FeatureUsageAnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, FeatureUsageGranularityHour, h.Granularity, "≤3 天推断为 hour")
	d, err := GetFeatureUsageAnalytics(7201, start, start+5*86400, "", FeatureUsageAnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, FeatureUsageGranularityDay, d.Granularity, "5 天推断为 day")
	w, err := GetFeatureUsageAnalytics(7201, start, start+90*86400, "", FeatureUsageAnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, FeatureUsageGranularityWeek, w.Granularity, "90 天推断为 week")
}

// TestFeatureUsageAnalytics_Filters 复用 seed 验证 feature_code / biz_task_id /
// task_keyword / operation_code 过滤与 totals/features 同口径。
func TestFeatureUsageAnalytics_Filters(t *testing.T) {
	baseTs := int64(1_700_001_000)
	seedFeatureUsageLogs(t, 7202, baseTs)
	window := FeatureUsageAnalyticsFilter{}

	t.Run("feature_code", func(t *testing.T) {
		res, err := GetFeatureUsageAnalytics(7202, baseTs, baseTs+1000, FeatureUsageGranularityDay,
			FeatureUsageAnalyticsFilter{FeatureCode: "image_creation"})
		require.NoError(t, err)
		assert.Equal(t, 200, res.Totals.Quota)
		assert.Equal(t, 1, res.Totals.RequestCount)
		require.Len(t, res.Features, 1)
		assert.Equal(t, "image_creation", res.Features[0].FeatureCode)
	})
	t.Run("biz_task_id", func(t *testing.T) {
		res, err := GetFeatureUsageAnalytics(7202, baseTs, baseTs+1000, FeatureUsageGranularityDay,
			FeatureUsageAnalyticsFilter{BizTaskID: "biz-1"})
		require.NoError(t, err)
		// merch/biz-1 净额 100(submit)+50(settle)-30(refund)=120；request_count 只计 submit=1。
		assert.Equal(t, 120, res.Totals.Quota)
		assert.Equal(t, 1, res.Totals.RequestCount)
		require.Len(t, res.Features, 1)
		assert.Equal(t, "merch_video_clone", res.Features[0].FeatureCode)
	})
	t.Run("operation_code", func(t *testing.T) {
		res, err := GetFeatureUsageAnalytics(7202, baseTs, baseTs+1000, FeatureUsageGranularityDay,
			FeatureUsageAnalyticsFilter{OperationCode: "image_creation.generate"})
		require.NoError(t, err)
		assert.Equal(t, 200, res.Totals.Quota)
		require.Len(t, res.Features, 1)
		assert.Equal(t, "image_creation", res.Features[0].FeatureCode)
	})
	t.Run("task_keyword", func(t *testing.T) {
		res, err := GetFeatureUsageAnalytics(7202, baseTs, baseTs+1000, FeatureUsageGranularityDay,
			FeatureUsageAnalyticsFilter{TaskKeyword: "带货视频"})
		require.NoError(t, err)
		assert.Equal(t, 120, res.Totals.Quota)
		require.Len(t, res.Features, 1)
		assert.Equal(t, "merch_video_clone", res.Features[0].FeatureCode)
	})
	// 零值 filter：全部归因日志（普通 API Key 消耗 9999 不入），净额 360。
	all, err := GetFeatureUsageAnalytics(7202, baseTs, baseTs+1000, FeatureUsageGranularityDay, window)
	require.NoError(t, err)
	assert.Equal(t, 360, all.Totals.Quota)
}

// TestFeatureUsageAnalytics_RefundNetTokenAndRates 计费口径：refund 净额倒扣 quota 但不倒扣 token；
// request_count 只计 request/submit；token_used 仅 consume 行计入；avg_rpm/avg_tpm 按窗口分钟数计算。
func TestFeatureUsageAnalytics_RefundNetTokenAndRates(t *testing.T) {
	baseTs := int64(1_700_002_000)
	logs := []*Log{
		// merch/biz-1 submit：consume, quota 100, token 1500, 计 request
		{UserId: 7203, CreatedAt: baseTs + 10, Type: LogTypeConsume, Quota: 100, PromptTokens: 1000, CompletionTokens: 500,
			Other: buildOtherJSON("merch_video_clone", "merch_video_clone.step3.full_video.segment_submit", "biz-1", "", common.WischoicerStageSubmit, "", "", "")},
		// merch/biz-1 settle：consume, quota 50, token 0, 不计 request
		{UserId: 7203, CreatedAt: baseTs + 20, Type: LogTypeConsume, Quota: 50,
			Other: buildOtherJSON("merch_video_clone", "merch_video_clone.step3.full_video.segment_submit", "biz-1", "", common.WischoicerStageSettle, "", "", "")},
		// merch/biz-1 refund：refund, quota 30, token 300（不计入），不计 request
		{UserId: 7203, CreatedAt: baseTs + 30, Type: LogTypeRefund, Quota: 30, PromptTokens: 200, CompletionTokens: 100,
			Other: buildOtherJSON("merch_video_clone", "merch_video_clone.step3.full_video.segment_submit", "biz-1", "", common.WischoicerStageRefund, "", "", "")},
		// image/biz-2 request：consume, quota 200, token 5000, 计 request
		{UserId: 7203, CreatedAt: baseTs + 40, Type: LogTypeConsume, Quota: 200, PromptTokens: 4000, CompletionTokens: 1000,
			Other: buildOtherJSON("image_creation", "image_creation.generate", "biz-2", "", common.WischoicerStageRequest, "", "", "")},
	}
	for _, l := range logs {
		require.NoError(t, LOG_DB.Create(l).Error)
	}
	// 窗口 600s = 10 整分钟。
	res, err := GetFeatureUsageAnalytics(7203, baseTs, baseTs+600, FeatureUsageGranularityHour, FeatureUsageAnalyticsFilter{})
	require.NoError(t, err)

	// quota 净额：100+50-30+200 = 320（refund 倒扣）。
	assert.Equal(t, 320, res.Totals.Quota)
	// request_count 只计 request/submit：submit + image request = 2（settle/refund 不计）。
	assert.Equal(t, 2, res.Totals.RequestCount)
	// token_used 仅 consume 行计入，refund 不倒扣：1500(submit)+0(settle)+5000(image) = 6500
	// （若 refund 倒扣 token 会是 6800）。
	assert.Equal(t, 6500, res.Totals.TokenUsed)
	// avg_rpm = 2/10 = 0.2；avg_tpm = 6500/10 = 650。
	assert.InDelta(t, 0.2, res.Totals.AvgRPM, 1e-9)
	assert.InDelta(t, 650.0, res.Totals.AvgTPM, 1e-9)

	// merch：净额 120、token 1500（submit 计入；settle 0；refund 不倒扣）。
	var merch FeatureUsageAnalyticsFeature
	for _, f := range res.Features {
		if f.FeatureCode == "merch_video_clone" {
			merch = f
		}
	}
	require.Equal(t, "merch_video_clone", merch.FeatureCode)
	assert.Equal(t, 120, merch.Quota)
	assert.Equal(t, 1, merch.RequestCount)
	assert.Equal(t, 1500, merch.TokenUsed)
}

// TestFeatureUsageAnalytics_UnknownFeatureAutoEnters 扩展性：未知新 feature_code（不在枚举）
// 自动成行并按快照名展示，不被折叠；缺 feature_code 归「其他功能消耗」。
func TestFeatureUsageAnalytics_UnknownFeatureAutoEnters(t *testing.T) {
	baseTs := int64(1_700_003_000)
	// 未来新板块：枚举外，带快照 feature_name。
	futureOther := common.MapToJsonStr(map[string]interface{}{
		"wischoicer": map[string]interface{}{
			"schema_version": 1, "source_service": "content-workstation", "internal_function": true,
			"feature_code": "future_feature_x", "feature_name": "未来新板块",
			"operation_code": "future.op", "biz_task_id": "biz-fx", "account_id": "acct-1", "app_user_id": "acct-1",
			"billing_stage": common.WischoicerStageRequest,
		},
	})
	logs := []*Log{
		{UserId: 7204, CreatedAt: baseTs + 10, Type: LogTypeConsume, Quota: 300,
			Other: futureOther},
		{UserId: 7204, CreatedAt: baseTs + 20, Type: LogTypeConsume, Quota: 100,
			Other: buildOtherJSON("merch_video_clone", "merch_video_clone.step3.full_video.segment_submit", "biz-m", "", common.WischoicerStageRequest, "", "", "")},
		// feature_code 缺失 → 其他功能消耗
		{UserId: 7204, CreatedAt: baseTs + 30, Type: LogTypeConsume, Quota: 40,
			Other: buildOtherJSON("", "some.unknown.op", "biz-u", "", common.WischoicerStageRequest, "", "", "")},
	}
	for _, l := range logs {
		require.NoError(t, LOG_DB.Create(l).Error)
	}

	res, err := GetFeatureUsageAnalytics(7204, baseTs, baseTs+1000, FeatureUsageGranularityDay, FeatureUsageAnalyticsFilter{})
	require.NoError(t, err)
	// 三个 feature 行，按 quota 降序：future(300) > merch(100) > 其他(40)。
	require.Len(t, res.Features, 3)
	assert.Equal(t, "future_feature_x", res.Features[0].FeatureCode)
	assert.Equal(t, "未来新板块", res.Features[0].FeatureName, "未知 code 回退日志快照名，不暴露技术文案")
	assert.Equal(t, 300, res.Features[0].Quota)
	assert.Equal(t, "merch_video_clone", res.Features[1].FeatureCode)
	assert.Equal(t, "爆款复刻中心 - 视频爆款复刻", res.Features[1].FeatureName)
	assert.Equal(t, "", res.Features[2].FeatureCode, "缺 feature_code 行 code 为空")
	assert.Equal(t, common.WischoicerOtherFeatureLabel, res.Features[2].FeatureName)
	assert.Equal(t, 40, res.Features[2].Quota)

	// 未知 feature 同样进入 points。
	var hasFuturePoint bool
	for _, p := range res.Points {
		if p.FeatureCode == "future_feature_x" {
			hasFuturePoint = true
			assert.Equal(t, "未来新板块", p.FeatureName)
		}
	}
	assert.True(t, hasFuturePoint, "未知 feature 必须出现在 points，无需改前端枚举")
}
