package controller

import (
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

// WIS-499 §4 费用明细 user-self 聚合查询接口。
//
// 公共规则：鉴权沿用 Authorization + New-Api-User（middleware.UserAuth，userId 在 c["id"]）；
// 时间 Unix 秒闭区间；单次窗口 ≤ 92 天；分页 p(1-based) + page_size(默认 20，最大 100)；
// cost_rmb = quota / 500000；聚合 quota 用净值（consume 正 / refund 负）；
// request_count 只计 billing_stage in (request, submit)。

const (
	featureUsageDefaultPageSize = 20
	featureUsageMaxPageSize     = 100
)

// parseFeatureUsageRange 解析并校验时间窗口。返回 (start, end, ok)。
func parseFeatureUsageRange(c *gin.Context) (int64, int64, bool) {
	start, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	end, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
	if start <= 0 || end <= 0 {
		common.ApiErrorMsg(c, "start_timestamp 和 end_timestamp 必填")
		return 0, 0, false
	}
	if start > end {
		common.ApiErrorMsg(c, "start_timestamp 不能晚于 end_timestamp")
		return 0, 0, false
	}
	if end-start > model.FeatureUsageMaxWindowSeconds {
		common.ApiErrorMsg(c, "查询窗口不得超过 92 天")
		return 0, 0, false
	}
	return start, end, true
}

// parseFeatureUsagePaging 解析分页参数。
func parseFeatureUsagePaging(c *gin.Context) (int, int) {
	page, _ := strconv.Atoi(c.Query("p"))
	if page < 1 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	if pageSize < 1 {
		pageSize = featureUsageDefaultPageSize
	}
	if pageSize > featureUsageMaxPageSize {
		pageSize = featureUsageMaxPageSize
	}
	return page, pageSize
}

// GetFeatureUsageSummary GET /api/log/self/feature-usage/summary（RFC §4.1）。
func GetFeatureUsageSummary(c *gin.Context) {
	userId := c.GetInt("id")
	start, end, ok := parseFeatureUsageRange(c)
	if !ok {
		return
	}
	featureCode := c.Query("feature_code")
	if featureCode == "uncategorized" {
		common.ApiErrorMsg(c, "summary 不支持 uncategorized，请传真实 feature_code")
		return
	}
	if featureCode != "" && !common.WischoicerIsKnownFeatureCode(featureCode) {
		common.ApiErrorMsg(c, "未知的 feature_code")
		return
	}
	result, err := model.GetFeatureUsageSummary(userId, start, end, model.FeatureUsageSummaryFilter{
		FeatureCode:   featureCode,
		BizTaskID:     c.Query("biz_task_id"),
		TaskKeyword:   c.Query("task_keyword"),
		OperationCode: c.Query("operation_code"),
	})
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, result)
}

// GetFeatureUsageTasks GET /api/log/self/feature-usage/tasks（RFC §4.2）。
func GetFeatureUsageTasks(c *gin.Context) {
	userId := c.GetInt("id")
	start, end, ok := parseFeatureUsageRange(c)
	if !ok {
		return
	}
	featureCode := c.Query("feature_code")
	if featureCode == "uncategorized" {
		common.ApiErrorMsg(c, "tasks 不支持 uncategorized")
		return
	}
	if featureCode != "" && !common.WischoicerIsKnownFeatureCode(featureCode) {
		common.ApiErrorMsg(c, "未知的 feature_code")
		return
	}
	page, pageSize := parseFeatureUsagePaging(c)
	result, err := model.GetFeatureUsageTasks(userId, start, end, model.FeatureUsageTasksFilter{
		FeatureCode:   featureCode,
		BizTaskID:     c.Query("biz_task_id"),
		TaskKeyword:   c.Query("task_keyword"),
		OperationCode: c.Query("operation_code"),
	}, page, pageSize)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, result)
}

// GetFeatureUsageDetails GET /api/log/self/feature-usage/details（RFC §4.3）。
func GetFeatureUsageDetails(c *gin.Context) {
	userId := c.GetInt("id")
	start, end, ok := parseFeatureUsageRange(c)
	if !ok {
		return
	}
	featureCode := c.Query("feature_code")
	// details 允许传 uncategorized；其他值必须是已知 feature_code。
	if featureCode != "" && featureCode != "uncategorized" && !common.WischoicerIsKnownFeatureCode(featureCode) {
		common.ApiErrorMsg(c, "未知的 feature_code")
		return
	}
	page, pageSize := parseFeatureUsagePaging(c)
	result, err := model.GetFeatureUsageDetails(userId, start, end, model.FeatureUsageDetailsFilter{
		FeatureCode:   featureCode,
		BizTaskID:     c.Query("biz_task_id"),
		TaskKeyword:   c.Query("task_keyword"),
		OperationCode: c.Query("operation_code"),
	}, page, pageSize)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, result)
}
