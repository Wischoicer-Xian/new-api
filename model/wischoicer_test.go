package model

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseWischoicerAttribution_NilCases(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// nil context → nil
	assert.Nil(t, ParseWischoicerAttribution(nil))

	// 有 request 但无任何 X-Wischoicer-* header → nil
	c, _ := gin.CreateTestContext(nil)
	c.Request = newGetRequestWithHeaders(nil)
	assert.Nil(t, ParseWischoicerAttribution(c))

	// 仅 source_service 但 internal_function=false → nil（不进入功能计费归因）
	c2, _ := gin.CreateTestContext(nil)
	c2.Request = newGetRequestWithHeaders(map[string]string{
		WischoicerHeaderSourceService:    WischoicerSourceServiceContentWorkstation,
		WischoicerHeaderInternalFunction: "false",
	})
	assert.Nil(t, ParseWischoicerAttribution(c2))

	// source_service 非 content-workstation → nil
	c3, _ := gin.CreateTestContext(nil)
	c3.Request = newGetRequestWithHeaders(map[string]string{
		WischoicerHeaderSourceService:    "other-service",
		WischoicerHeaderInternalFunction: "true",
	})
	assert.Nil(t, ParseWischoicerAttribution(c3))
}

func TestParseWischoicerAttribution_Normalization(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// account_id 缺失 → effective_account_id == app_user_id
	c, _ := gin.CreateTestContext(nil)
	c.Request = newGetRequestWithHeaders(map[string]string{
		WischoicerHeaderSourceService:    WischoicerSourceServiceContentWorkstation,
		WischoicerHeaderInternalFunction: "true",
		WischoicerHeaderFeatureCode:      FeatureCodeMerchVideoClone,
		WischoicerHeaderOperationCode:    "merch_video_clone.step1.analyze",
		WischoicerHeaderBizTaskId:        "task-abc",
		WischoicerHeaderAppUserId:        "user-123",
		WischoicerHeaderFeatureName:      "带货视频爆款复刻",
	})
	w := ParseWischoicerAttribution(c)
	require.NotNil(t, w)
	assert.Equal(t, "user-123", w.AccountId, "account_id 缺失时应归一化为 app_user_id")
	assert.Equal(t, "user-123", w.AppUserId)
	assert.Equal(t, FeatureCodeMerchVideoClone, w.FeatureCode)
	assert.True(t, w.InternalFunction)
	assert.Equal(t, 1, w.SchemaVersion)

	// account_id 与 app_user_id 同时存在 → effective_account_id == account_id（v1 MVP 同值）
	c2, _ := gin.CreateTestContext(nil)
	c2.Request = newGetRequestWithHeaders(map[string]string{
		WischoicerHeaderSourceService:    WischoicerSourceServiceContentWorkstation,
		WischoicerHeaderInternalFunction: "true",
		WischoicerHeaderFeatureCode:      FeatureCodeImageCreation,
		WischoicerHeaderOperationCode:    "image_creation.generate",
		WischoicerHeaderBizTaskId:        "task-def",
		WischoicerHeaderAccountId:        "acct-456",
		WischoicerHeaderAppUserId:        "user-456",
	})
	w2 := ParseWischoicerAttribution(c2)
	require.NotNil(t, w2)
	assert.Equal(t, "acct-456", w2.AccountId, "account_id 存在时 effective_account_id 取 account_id")
	assert.Equal(t, "user-456", w2.AppUserId)
}

func TestWischoicerAttribution_ToMap_Structure(t *testing.T) {
	w := &WischoicerAttribution{
		SchemaVersion:    1,
		SourceService:    WischoicerSourceServiceContentWorkstation,
		InternalFunction: true,
		FeatureCode:      FeatureCodeReferenceCopy,
		FeatureName:      "图文复刻",
		OperationCode:    "reference_copy.analyze",
		BizTaskId:        "t1",
		AccountId:        "a1",
		AppUserId:        "u1",
	}
	m := w.ToMap(BillingStageRequest)
	assert.Equal(t, 1, m["schema_version"])
	assert.Equal(t, WischoicerSourceServiceContentWorkstation, m["source_service"])
	assert.Equal(t, true, m["internal_function"])
	assert.Equal(t, FeatureCodeReferenceCopy, m["feature_code"])
	assert.Equal(t, "reference_copy.analyze", m["operation_code"])
	assert.Equal(t, "t1", m["biz_task_id"])
	assert.Equal(t, "a1", m["account_id"])
	assert.Equal(t, "u1", m["app_user_id"])
	assert.Equal(t, BillingStageRequest, m["billing_stage"])
	assert.Equal(t, "图文复刻", m["feature_name"])
	// optional 字段未设时不出现
	assert.NotContains(t, m, "biz_task_title")
	assert.NotContains(t, m, "sub_task_id")
}

// seedWischoicerLog 写入一条带 wischoicer 归因的日志（type=consume/refund）。
// otherExtra 用于追加 request_id / task_id 等 settle/refund 快照字段。
func seedWischoicerLog(t *testing.T, userId int, createdAt int64, logType int, quota int,
	modelName string, w *WischoicerAttribution, billingStage string, otherExtra map[string]interface{}) {
	t.Helper()
	other := map[string]interface{}{}
	if w != nil {
		other["wischoicer"] = w.ToMap(billingStage)
	}
	for k, v := range otherExtra {
		other[k] = v
	}
	log := &Log{
		UserId:    userId,
		CreatedAt: createdAt,
		Type:      logType,
		ModelName: modelName,
		Quota:     quota,
		Other:     common.MapToJsonStr(other),
	}
	require.NoError(t, DB.Create(log).Error)
}

func newGetRequestWithHeaders(headers map[string]string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

func TestFeatureUsageAggregation(t *testing.T) {
	// 复用 model 包 TestMain 的内存 SQLite。清理可能存在的旧数据。
	require.NoError(t, DB.Exec("DELETE FROM logs").Error)
	userId := 9001
	base := int64(1717000000) // 固定起点，避免时间窗口校验问题

	// feature=merch_video_clone, biz_task=t1, 同一任务 4 个阶段
	wMVC := &WischoicerAttribution{
		SchemaVersion: 1, SourceService: WischoicerSourceServiceContentWorkstation,
		InternalFunction: true, FeatureCode: FeatureCodeMerchVideoClone,
		FeatureName: "复刻", OperationCode: "merch_video_clone.step1.analyze", BizTaskId: "t1",
		AccountId: "u1", AppUserId: "u1",
	}
	// request 阶段（顶层 request_id 列）
	seedWischoicerLog(t, userId, base+10, LogTypeConsume, 500000, "gemini", wMVC, BillingStageRequest, nil)
	// submit 阶段（顶层 request_id 列 + other.task_id）
	seedWischoicerLog(t, userId, base+20, LogTypeConsume, 1000000, "seedance", wMVC, BillingStageSubmit,
		map[string]interface{}{"task_id": "task_xxx"})
	// settle 阶段（RecordTaskBillingLog 无 request_id 列；other 携带 submit 快照）
	seedWischoicerLog(t, userId, base+30, LogTypeConsume, 500000, "seedance", wMVC, BillingStageSettle,
		map[string]interface{}{"task_id": "task_xxx", "request_id": "rid-submit", "upstream_request_id": "urid-submit"})
	// refund 阶段
	seedWischoicerLog(t, userId, base+40, LogTypeRefund, 250000, "seedance", wMVC, BillingStageRefund,
		map[string]interface{}{"task_id": "task_xxx", "request_id": "rid-submit", "upstream_request_id": "urid-submit"})

	// feature=image_creation, biz_task=t2
	wIMG := &WischoicerAttribution{
		SchemaVersion: 1, SourceService: WischoicerSourceServiceContentWorkstation,
		InternalFunction: true, FeatureCode: FeatureCodeImageCreation,
		OperationCode: "image_creation.generate", BizTaskId: "t2",
		AccountId: "u1", AppUserId: "u1",
	}
	seedWischoicerLog(t, userId, base+50, LogTypeConsume, 500000, "gpt-image", wIMG, BillingStageRequest, nil)

	// uncategorized: source_service=content-workstation + internal_function=true 但 feature_code 空
	wUnc := &WischoicerAttribution{
		SchemaVersion: 1, SourceService: WischoicerSourceServiceContentWorkstation,
		InternalFunction: true, FeatureCode: "", OperationCode: "unknown", BizTaskId: "",
		AccountId: "u1", AppUserId: "u1",
	}
	seedWischoicerLog(t, userId, base+60, LogTypeConsume, 100000, "some-model", wUnc, BillingStageRequest, nil)

	// 不计入：无 wischoicer 归因（普通 API Key 调用）
	seedWischoicerLog(t, userId, base+70, LogTypeConsume, 999999, "gpt-4", nil, "", nil)
	// 不计入：internal_function=false
	wExt := &WischoicerAttribution{
		SchemaVersion: 1, SourceService: WischoicerSourceServiceContentWorkstation,
		InternalFunction: false, FeatureCode: FeatureCodeImageCreation,
		AccountId: "u1", AppUserId: "u1",
	}
	seedWischoicerLog(t, userId, base+80, LogTypeConsume, 888888, "gpt-4", wExt, BillingStageRequest, nil)

	start, end := base, base+100

	// --- summary ---
	summary, err := GetFeatureUsageSummary(userId, start, end, "")
	require.NoError(t, err)
	// totals: 500k+1000k+500k-250k+500k+100k = 2350k
	assert.Equal(t, 2350000, summary.Totals.Quota)
	assert.InDelta(t, 4.7, summary.Totals.CostRmb, 1e-9)
	// request_count: request+submit 阶段 = log1,log2,log5,log6 = 4
	assert.Equal(t, 4, summary.Totals.RequestCount)
	// uncategorized
	assert.True(t, summary.Uncategorized.Present)
	assert.Equal(t, 100000, summary.Uncategorized.Quota)
	assert.Equal(t, 1, summary.Uncategorized.RequestCount)
	// features
	require.Len(t, summary.Features, 2)
	// 按 last_seen desc：image_creation (base+50) vs merch_video_clone (base+40) → image first
	assert.Equal(t, FeatureCodeImageCreation, summary.Features[0].FeatureCode)
	assert.Equal(t, 1, summary.Features[0].TaskCount)
	assert.Equal(t, 500000, summary.Features[0].Quota)
	assert.Equal(t, FeatureCodeMerchVideoClone, summary.Features[1].FeatureCode)
	assert.Equal(t, 1, summary.Features[1].TaskCount)
	// merch_video_clone: request+submit = log1+log2 = 2；净值 quota = 500k+1000k+500k-250k = 1750k
	assert.Equal(t, 2, summary.Features[1].RequestCount)
	assert.Equal(t, 1750000, summary.Features[1].Quota)
	// feature_name 走 canonical map（不被快照 "复刻" 污染）
	assert.Equal(t, "爆款复刻中心 - 带货视频爆款复刻", summary.Features[1].FeatureName)

	// --- tasks ---
	tasks, err := GetFeatureUsageTasks(FeatureUsageTasksQuery{UserId: userId, StartTs: start, EndTs: end, Page: 1, PageSize: 20})
	require.NoError(t, err)
	assert.Equal(t, 2, tasks.Total)
	require.Len(t, tasks.Items, 2)
	// 每个任务 1 条；t1 净值 1750k，t2 净值 500k
	var t1Item, t2Item *FeatureUsageTaskItem
	for i := range tasks.Items {
		switch tasks.Items[i].BizTaskId {
		case "t1":
			t1Item = &tasks.Items[i]
		case "t2":
			t2Item = &tasks.Items[i]
		}
	}
	require.NotNil(t, t1Item)
	require.NotNil(t, t2Item)
	assert.Equal(t, 1750000, t1Item.Quota)
	assert.Equal(t, 2, t1Item.RequestCount)
	assert.Equal(t, 500000, t2Item.Quota)
	assert.Equal(t, 1, t2Item.RequestCount)

	// --- details（merch_video_clone）---
	details, err := GetFeatureUsageDetails(FeatureUsageDetailsQuery{UserId: userId, StartTs: start, EndTs: end, FeatureCode: FeatureCodeMerchVideoClone, Page: 1, PageSize: 20})
	require.NoError(t, err)
	assert.Equal(t, 4, details.Total)
	require.Len(t, details.Items, 4)
	// 按 created_at desc：refund(base+40) > settle(base+30) > submit(base+20) > request(base+10)
	refundItem := details.Items[0]
	assert.Equal(t, BillingStageRefund, refundItem.BillingStage)
	assert.Equal(t, "refund", refundItem.LogType)
	assert.Equal(t, -250000, refundItem.Quota, "refund 行 quota 取负")
	// settle/refund 复用 submit 快照 request_id
	require.NotNil(t, refundItem.RequestId)
	assert.Equal(t, "rid-submit", *refundItem.RequestId)
	require.NotNil(t, refundItem.UpstreamRequestId)
	assert.Equal(t, "urid-submit", *refundItem.UpstreamRequestId)
	require.NotNil(t, refundItem.ProviderTaskId)
	assert.Equal(t, "task_xxx", *refundItem.ProviderTaskId)

	settleItem := details.Items[1]
	assert.Equal(t, BillingStageSettle, settleItem.BillingStage)
	require.NotNil(t, settleItem.RequestId)
	assert.Equal(t, "rid-submit", *settleItem.RequestId)

	// request/submit 阶段取顶层列（此处顶层列为空，因为 seedWischoicerLog 未设 RequestId 列）
	submitItem := details.Items[2]
	assert.Equal(t, BillingStageSubmit, submitItem.BillingStage)
	// 顶层 RequestId 为空 → null
	assert.Nil(t, submitItem.RequestId)
	// submit 有 other.task_id → provider_task_id 非空
	require.NotNil(t, submitItem.ProviderTaskId)
	assert.Equal(t, "task_xxx", *submitItem.ProviderTaskId)

	// --- details uncategorized ---
	detailsUnc, err := GetFeatureUsageDetails(FeatureUsageDetailsQuery{UserId: userId, StartTs: start, EndTs: end, FeatureCode: "uncategorized", Page: 1, PageSize: 20})
	require.NoError(t, err)
	assert.Equal(t, 1, detailsUnc.Total)
	require.Len(t, detailsUnc.Items, 1)
	assert.Equal(t, "", detailsUnc.Items[0].FeatureCode)
}

func TestFeatureUsageWindowValidation(t *testing.T) {
	require.ErrorIs(t, ValidateWischoicerFeatureUsageWindow(0, 100), errWischoicerTimeRequired)
	require.ErrorIs(t, ValidateWischoicerFeatureUsageWindow(100, 0), errWischoicerTimeRequired)
	require.ErrorIs(t, ValidateWischoicerFeatureUsageWindow(200, 100), errWischoicerTimeRange)
	// 93 天 > 92 天上限
	start := int64(1717000000)
	end := start + 93*24*3600
	require.ErrorIs(t, ValidateWischoicerFeatureUsageWindow(start, end), errWischoicerWindowTooWide)
	// 92 天合法
	end92 := start + 92*24*3600
	require.NoError(t, ValidateWischoicerFeatureUsageWindow(start, end92))
}

func TestFeatureUsagePagination(t *testing.T) {
	require.NoError(t, DB.Exec("DELETE FROM logs").Error)
	userId := 9002
	base := int64(1717000000)
	// 造 25 条 image_creation request 日志，biz_task 各不同
	for i := 0; i < 25; i++ {
		w := &WischoicerAttribution{
			SchemaVersion: 1, SourceService: WischoicerSourceServiceContentWorkstation,
			InternalFunction: true, FeatureCode: FeatureCodeImageCreation,
			OperationCode: "image_creation.generate", BizTaskId: "tk-" + string(rune('a'+i)),
			AccountId: "u", AppUserId: "u",
		}
		seedWischoicerLog(t, userId, base+int64(i), LogTypeConsume, 100000, "gpt-image", w, BillingStageRequest, nil)
	}
	start, end := base, base+100
	// page_size=10 → 第 1 页 10 条，total 25
	tasks, err := GetFeatureUsageTasks(FeatureUsageTasksQuery{UserId: userId, StartTs: start, EndTs: end, Page: 1, PageSize: 10})
	require.NoError(t, err)
	assert.Equal(t, 25, tasks.Total)
	require.Len(t, tasks.Items, 10)
	// page=3 → 5 条
	tasks3, err := GetFeatureUsageTasks(FeatureUsageTasksQuery{UserId: userId, StartTs: start, EndTs: end, Page: 3, PageSize: 10})
	require.NoError(t, err)
	assert.Equal(t, 25, tasks3.Total)
	require.Len(t, tasks3.Items, 5)
	// page=100 越界 → 0 条，total 仍 25
	tasksOOB, err := GetFeatureUsageTasks(FeatureUsageTasksQuery{UserId: userId, StartTs: start, EndTs: end, Page: 100, PageSize: 10})
	require.NoError(t, err)
	assert.Equal(t, 25, tasksOOB.Total)
	require.Len(t, tasksOOB.Items, 0)
}
