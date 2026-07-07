package controller

import (
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

// ---------------------------------------------------------------------------
// Wischoicer 费用明细查询接口（WIS-499 RFC §4）
//
// 三个 user-self 接口：summary / tasks / details。鉴权沿用现有
// Authorization + New-Api-User（UserAuth 中间件），不新增认证协议。
// ---------------------------------------------------------------------------

const (
	wischoicerFeatureUsageDefaultPageSize = 20
	wischoicerFeatureUsageMaxPageSize     = 100
)

// parseFeatureUsagePaging 解析分页参数：p(1-based) + page_size，默认 20，最大 100。
func parseFeatureUsagePaging(c *gin.Context) (int, int) {
	page, _ := strconv.Atoi(c.Query("p"))
	if page < 1 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	if pageSize < 1 {
		pageSize = wischoicerFeatureUsageDefaultPageSize
	}
	if pageSize > wischoicerFeatureUsageMaxPageSize {
		pageSize = wischoicerFeatureUsageMaxPageSize
	}
	return page, pageSize
}

// parseFeatureUsageWindow 解析并校验时间窗口（Unix 秒，闭区间，<= 92 天）。
func parseFeatureUsageWindow(c *gin.Context) (int64, int64, error) {
	startTs, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	endTs, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
	if err := model.ValidateWischoicerFeatureUsageWindow(startTs, endTs); err != nil {
		return 0, 0, err
	}
	return startTs, endTs, nil
}

// GetFeatureUsageSummary GET /api/log/self/feature-usage/summary
func GetFeatureUsageSummary(c *gin.Context) {
	userId := c.GetInt("id")
	startTs, endTs, err := parseFeatureUsageWindow(c)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	featureCode := c.Query("feature_code")
	// summary 不允许 uncategorized；未知 code 视为无匹配（model 层已处理）
	summary, err := model.GetFeatureUsageSummary(userId, startTs, endTs, featureCode)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, summary)
}

// GetFeatureUsageTasks GET /api/log/self/feature-usage/tasks
func GetFeatureUsageTasks(c *gin.Context) {
	userId := c.GetInt("id")
	startTs, endTs, err := parseFeatureUsageWindow(c)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	page, pageSize := parseFeatureUsagePaging(c)
	featureCode := c.Query("feature_code")
	// tasks 不支持 uncategorized；未知 code 直接返回空结果
	if featureCode != "" && (featureCode == "uncategorized" || !model.IsKnownWischoicerFeatureCode(featureCode)) {
		common.ApiSuccess(c, &model.FeatureUsageTasksResult{
			Page:     page,
			PageSize: pageSize,
			Total:    0,
			Items:    []model.FeatureUsageTaskItem{},
		})
		return
	}
	q := model.FeatureUsageTasksQuery{
		UserId:        userId,
		StartTs:       startTs,
		EndTs:         endTs,
		FeatureCode:   featureCode,
		BizTaskId:     c.Query("biz_task_id"),
		TaskKeyword:   c.Query("task_keyword"),
		OperationCode: c.Query("operation_code"),
		Page:          page,
		PageSize:      pageSize,
	}
	result, err := model.GetFeatureUsageTasks(q)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, result)
}

// GetFeatureUsageDetails GET /api/log/self/feature-usage/details
func GetFeatureUsageDetails(c *gin.Context) {
	userId := c.GetInt("id")
	startTs, endTs, err := parseFeatureUsageWindow(c)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	page, pageSize := parseFeatureUsagePaging(c)
	featureCode := c.Query("feature_code")
	// details 允许传 uncategorized；其它未知 code 视为无匹配
	if featureCode != "" && featureCode != "uncategorized" && !model.IsKnownWischoicerFeatureCode(featureCode) {
		common.ApiSuccess(c, &model.FeatureUsageDetailsResult{
			Page:     page,
			PageSize: pageSize,
			Total:    0,
			Items:    []model.FeatureUsageDetailItem{},
		})
		return
	}
	q := model.FeatureUsageDetailsQuery{
		UserId:        userId,
		StartTs:       startTs,
		EndTs:         endTs,
		FeatureCode:   featureCode,
		BizTaskId:     c.Query("biz_task_id"),
		OperationCode: c.Query("operation_code"),
		Page:          page,
		PageSize:      pageSize,
	}
	result, err := model.GetFeatureUsageDetails(q)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, result)
}
