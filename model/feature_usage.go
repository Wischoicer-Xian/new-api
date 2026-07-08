package model

import (
	"sort"
	"strings"

	"github.com/QuantumNous/new-api/common"
)

// WIS-499 §4 费用明细聚合查询（user-self）。
//
// 设计前提（RFC §7）：v1 不做 schema migration / 新表 / 新索引，基于现有 logs.other JSON。
// 跨库兼容（SQLite/MySQL/PostgreSQL）：不走任一家专属 JSON 索引，SQL 层只用 user_id +
// created_at + type 过滤并配合 other LIKE 粗筛，命中的归因日志再在 Go 层精解析聚合。
// 性能边界：user-self + ≤92 天窗口 + 不承诺索引级性能；命中门槛再升 R3。

const (
	// FeatureUsageMaxWindowSeconds 单次查询窗口上限：92 天。
	FeatureUsageMaxWindowSeconds = 92 * 24 * 60 * 60
	// other LIKE 粗筛模式：只捞带 wischoicer 归因对象的日志，剔除普通 API Key 消耗。
	wischoicerOtherLikePattern = `%"wischoicer"%`
)

// featureUsageRow 一条已归因日志的解析视图。
type featureUsageRow struct {
	Log           *Log
	FeatureCode   string
	FeatureName   string
	OperationCode string
	OperationName string
	BizTaskID     string
	BizTaskTitle  string
	SubTaskID     string
	AccountID     string
	BillingStage  string
	// ProviderTaskID 取 other.provider_task_id（上游真实任务 ID，仅异步任务日志有；request 阶段为空）。
	ProviderTaskID string
	// SnapRequestID / SnapUpstreamID 为 settle/refund 日志从 submit 快照复用的链路 ID。
	SnapRequestID  string
	SnapUpstreamID string
}

// featureUsageCostRMB 按 RFC 口径 quota / 500000（= common.QuotaPerUnit）换算 RMB。
func featureUsageCostRMB(signedQuota int) float64 {
	return float64(signedQuota) / common.QuotaPerUnit
}

// featureUsageSignedQuota 净值：consume 正，refund 负。
func featureUsageSignedQuota(lg *Log) int {
	if lg.Type == LogTypeRefund {
		return -lg.Quota
	}
	return lg.Quota
}

// featureUsageCountsAsRequest 是否计入 request_count（仅 request/submit 原始调用）。
func featureUsageCountsAsRequest(billingStage string) bool {
	return billingStage == common.WischoicerStageRequest || billingStage == common.WischoicerStageSubmit
}

// loadSelfAttributedLogs 拉取某用户时间窗口内全部带 wischoicer 归因的消费/退款日志，
// 并在 Go 层精解析为 featureUsageRow。LIKE 粗筛 + IsValid 双重保证普通 API Key 消耗不混入。
func loadSelfAttributedLogs(userId int, startTimestamp, endTimestamp int64) ([]featureUsageRow, error) {
	var logs []*Log
	err := LOG_DB.
		Where("user_id = ? AND created_at >= ? AND created_at <= ? AND type IN ? AND other LIKE ?",
			userId, startTimestamp, endTimestamp, []int{LogTypeConsume, LogTypeRefund}, wischoicerOtherLikePattern).
		Find(&logs).Error
	if err != nil {
		common.SysError("failed to load attributed logs: " + err.Error())
		return nil, err
	}
	rows := make([]featureUsageRow, 0, len(logs))
	for _, lg := range logs {
		otherMap, _ := common.StrToMap(lg.Other)
		if otherMap == nil {
			continue
		}
		w, ok := otherMap["wischoicer"].(map[string]interface{})
		if !ok {
			continue
		}
		attr := common.WischoicerAttributionFromMap(w)
		if attr == nil {
			// LIKE 粗筛后的防御性二次校验：source/internal 不符不算归因。
			continue
		}
		rows = append(rows, featureUsageRow{
			Log:            lg,
			FeatureCode:    attr.FeatureCode,
			FeatureName:    attr.FeatureName,
			OperationCode:  attr.OperationCode,
			OperationName:  attr.OperationName,
			BizTaskID:      attr.BizTaskID,
			BizTaskTitle:   attr.BizTaskTitle,
			SubTaskID:      attr.SubTaskID,
			AccountID:      attr.AccountID,
			BillingStage:   common.WischoicerMapString(w, "billing_stage"),
			ProviderTaskID: common.WischoicerMapString(otherMap, "provider_task_id"),
			SnapRequestID:  common.WischoicerMapString(w, "request_id"),
			SnapUpstreamID: common.WischoicerMapString(w, "upstream_request_id"),
		})
	}
	return rows, nil
}

// FeatureUsageTotals summary 顶部合计。
type FeatureUsageTotals struct {
	Quota        int     `json:"quota"`
	CostRMB      float64 `json:"cost_rmb"`
	RequestCount int     `json:"request_count"`
}

// FeatureUsageFeatureAgg summary 按功能板块聚合行。
type FeatureUsageFeatureAgg struct {
	FeatureCode  string  `json:"feature_code"`
	FeatureName  string  `json:"feature_name"`
	TaskCount    int     `json:"task_count"`
	RequestCount int     `json:"request_count"`
	Quota        int     `json:"quota"`
	CostRMB      float64 `json:"cost_rmb"`
	FirstSeen    int64   `json:"first_seen"`
	LastSeen     int64   `json:"last_seen"`
}

// FeatureUsageUncategorized summary 未归类合计。
type FeatureUsageUncategorized struct {
	Present      bool    `json:"present"`
	Quota        int     `json:"quota"`
	CostRMB      float64 `json:"cost_rmb"`
	RequestCount int     `json:"request_count"`
}

// FeatureUsageSummaryResult summary 接口响应 data。
type FeatureUsageSummaryResult struct {
	Totals        FeatureUsageTotals        `json:"totals"`
	Features      []FeatureUsageFeatureAgg  `json:"features"`
	Uncategorized FeatureUsageUncategorized `json:"uncategorized"`
}

// GetFeatureUsageSummary 聚合功能板块费用概览（RFC §4.1）。
// featureCode 非空时仅返回该功能（且 uncategorized 不适用）；为空时返回全部已知功能 + uncategorized。
func GetFeatureUsageSummary(userId int, startTimestamp, endTimestamp int64, featureCode string) (FeatureUsageSummaryResult, error) {
	result := FeatureUsageSummaryResult{Features: []FeatureUsageFeatureAgg{}}
	rows, err := loadSelfAttributedLogs(userId, startTimestamp, endTimestamp)
	if err != nil {
		return result, err
	}

	type featAcc struct {
		name         string
		tasks        map[string]struct{}
		requestCount int
		quota        int
		first        int64
		last         int64
	}
	feats := make(map[string]*featAcc)
	var totals FeatureUsageTotals
	var unc FeatureUsageUncategorized

	for _, r := range rows {
		// 指定 feature_code 时，只统计该 feature 范围；uncategorized 与其它 feature 不进 totals。
		if featureCode != "" && r.FeatureCode != featureCode {
			continue
		}
		signed := featureUsageSignedQuota(r.Log)
		totals.Quota += signed
		totals.CostRMB = featureUsageCostRMB(totals.Quota)
		if featureUsageCountsAsRequest(r.BillingStage) {
			totals.RequestCount++
		}
		seen := r.Log.CreatedAt

		if !common.WischoicerIsKnownFeatureCode(r.FeatureCode) {
			// feature_code 缺失/未知但归因有效 → uncategorized（仅 featureCode 为空时可达）。
			unc.Present = true
			unc.Quota += signed
			unc.CostRMB = featureUsageCostRMB(unc.Quota)
			if featureUsageCountsAsRequest(r.BillingStage) {
				unc.RequestCount++
			}
			continue
		}
		acc, ok := feats[r.FeatureCode]
		if !ok {
			acc = &featAcc{tasks: map[string]struct{}{}, name: common.WischoicerFeatureDisplayName(r.FeatureCode)}
			feats[r.FeatureCode] = acc
		}
		if acc.name == "" {
			acc.name = r.FeatureName // 回退到日志快照展示名
		}
		if r.BizTaskID != "" {
			acc.tasks[r.BizTaskID] = struct{}{}
		}
		acc.quota += signed
		if featureUsageCountsAsRequest(r.BillingStage) {
			acc.requestCount++
		}
		if acc.first == 0 || seen < acc.first {
			acc.first = seen
		}
		if seen > acc.last {
			acc.last = seen
		}
	}

	for code, acc := range feats {
		result.Features = append(result.Features, FeatureUsageFeatureAgg{
			FeatureCode:  code,
			FeatureName:  acc.name,
			TaskCount:    len(acc.tasks),
			RequestCount: acc.requestCount,
			Quota:        acc.quota,
			CostRMB:      featureUsageCostRMB(acc.quota),
			FirstSeen:    acc.first,
			LastSeen:     acc.last,
		})
	}
	sort.Slice(result.Features, func(i, j int) bool {
		if result.Features[i].Quota != result.Features[j].Quota {
			return result.Features[i].Quota > result.Features[j].Quota
		}
		return result.Features[i].FeatureCode < result.Features[j].FeatureCode
	})
	result.Totals = totals
	result.Uncategorized = unc
	return result, nil
}

// FeatureUsageTaskAgg tasks 接口按 feature_code + biz_task_id 聚合行。
type FeatureUsageTaskAgg struct {
	BizTaskID    string  `json:"biz_task_id"`
	BizTaskTitle string  `json:"biz_task_title"`
	FeatureCode  string  `json:"feature_code"`
	FeatureName  string  `json:"feature_name"`
	RequestCount int     `json:"request_count"`
	Quota        int     `json:"quota"`
	CostRMB      float64 `json:"cost_rmb"`
	FirstSeen    int64   `json:"first_seen"`
	LastSeen     int64   `json:"last_seen"`
}

// FeatureUsageTasksResult tasks 接口响应 data。
type FeatureUsageTasksResult struct {
	Page     int                   `json:"page"`
	PageSize int                   `json:"page_size"`
	Total    int                   `json:"total"`
	Items    []FeatureUsageTaskAgg `json:"items"`
}

// FeatureUsageTasksFilter tasks 接口过滤参数。
type FeatureUsageTasksFilter struct {
	FeatureCode   string
	BizTaskID     string
	TaskKeyword   string // 匹配 biz_task_title（大小写不敏感包含）
	OperationCode string
}

// GetFeatureUsageTasks 按业务任务聚合（RFC §4.2）。聚合键 feature_code + biz_task_id，不支持 uncategorized。
func GetFeatureUsageTasks(userId int, startTimestamp, endTimestamp int64, filter FeatureUsageTasksFilter, page, pageSize int) (FeatureUsageTasksResult, error) {
	result := FeatureUsageTasksResult{Page: page, PageSize: pageSize, Items: []FeatureUsageTaskAgg{}}
	rows, err := loadSelfAttributedLogs(userId, startTimestamp, endTimestamp)
	if err != nil {
		return result, err
	}

	type taskAcc struct {
		bizTaskID    string
		featureCode  string
		featureName  string
		title        string
		requestCount int
		quota        int
		first        int64
		last         int64
	}
	tasks := make(map[string]*taskAcc)
	for _, r := range rows {
		if !common.WischoicerIsKnownFeatureCode(r.FeatureCode) {
			continue // tasks 不支持 uncategorized
		}
		if filter.FeatureCode != "" && r.FeatureCode != filter.FeatureCode {
			continue
		}
		if filter.BizTaskID != "" && r.BizTaskID != filter.BizTaskID {
			continue
		}
		if filter.OperationCode != "" && r.OperationCode != filter.OperationCode {
			continue
		}
		if filter.TaskKeyword != "" && !containsFold(r.BizTaskTitle, filter.TaskKeyword) {
			continue
		}
		key := r.FeatureCode + "\x00" + r.BizTaskID
		acc, ok := tasks[key]
		if !ok {
			acc = &taskAcc{
				bizTaskID:   r.BizTaskID,
				featureCode: r.FeatureCode,
				featureName: common.WischoicerFeatureDisplayName(r.FeatureCode),
			}
			tasks[key] = acc
		}
		if acc.featureName == "" {
			acc.featureName = r.FeatureName
		}
		if acc.title == "" {
			acc.title = r.BizTaskTitle
		}
		acc.quota += featureUsageSignedQuota(r.Log)
		if featureUsageCountsAsRequest(r.BillingStage) {
			acc.requestCount++
		}
		if acc.first == 0 || r.Log.CreatedAt < acc.first {
			acc.first = r.Log.CreatedAt
		}
		if r.Log.CreatedAt > acc.last {
			acc.last = r.Log.CreatedAt
		}
	}

	items := make([]FeatureUsageTaskAgg, 0, len(tasks))
	for _, acc := range tasks {
		items = append(items, FeatureUsageTaskAgg{
			BizTaskID:    acc.bizTaskID,
			BizTaskTitle: acc.title,
			FeatureCode:  acc.featureCode,
			FeatureName:  acc.featureName,
			RequestCount: acc.requestCount,
			Quota:        acc.quota,
			CostRMB:      featureUsageCostRMB(acc.quota),
			FirstSeen:    acc.first,
			LastSeen:     acc.last,
		})
	}
	// 按 last_seen 倒序，相同则按 first_seen 倒序，保证分页稳定。
	sort.Slice(items, func(i, j int) bool {
		if items[i].LastSeen != items[j].LastSeen {
			return items[i].LastSeen > items[j].LastSeen
		}
		return items[i].FirstSeen > items[j].FirstSeen
	})
	result.Total = len(items)
	start := (page - 1) * pageSize
	if start >= len(items) {
		return result, nil
	}
	end := start + pageSize
	if end > len(items) {
		end = len(items)
	}
	result.Items = items[start:end]
	return result, nil
}

// FeatureUsageDetailItem details 接口单条明细。
type FeatureUsageDetailItem struct {
	CreatedAt         int64   `json:"created_at"`
	FeatureCode       string  `json:"feature_code"`
	FeatureName       string  `json:"feature_name"`
	BizTaskID         string  `json:"biz_task_id"`
	BizTaskTitle      string  `json:"biz_task_title"`
	OperationCode     string  `json:"operation_code"`
	OperationName     string  `json:"operation_name"`
	SubTaskID         string  `json:"sub_task_id"`
	BillingStage      string  `json:"billing_stage"`
	LogType           string  `json:"log_type"`
	ModelName         string  `json:"model_name"`
	TokenName         string  `json:"token_name"`
	TokenID           int     `json:"token_id"`
	Quota             int     `json:"quota"`
	CostRMB           float64 `json:"cost_rmb"`
	PromptTokens      int     `json:"prompt_tokens"`
	CompletionTokens  int     `json:"completion_tokens"`
	RequestID         *string `json:"request_id"`
	UpstreamRequestID *string `json:"upstream_request_id"`
	ProviderTaskID    *string `json:"provider_task_id"`
}

// FeatureUsageDetailsResult details 接口响应 data。
type FeatureUsageDetailsResult struct {
	Page     int                      `json:"page"`
	PageSize int                      `json:"page_size"`
	Total    int                      `json:"total"`
	Items    []FeatureUsageDetailItem `json:"items"`
}

// FeatureUsageDetailsFilter details 接口过滤参数。
// FeatureCode 允许传 "uncategorized"（只看未归类）或具体 feature code。
type FeatureUsageDetailsFilter struct {
	FeatureCode   string
	BizTaskID     string
	OperationCode string
}

// GetFeatureUsageDetails 返回明细列表（RFC §4.3）。
// request_id/upstream_request_id：request/submit 取日志顶层列；settle/refund 复用 submit 快照，无则 null。
func GetFeatureUsageDetails(userId int, startTimestamp, endTimestamp int64, filter FeatureUsageDetailsFilter, page, pageSize int) (FeatureUsageDetailsResult, error) {
	result := FeatureUsageDetailsResult{Page: page, PageSize: pageSize, Items: []FeatureUsageDetailItem{}}
	rows, err := loadSelfAttributedLogs(userId, startTimestamp, endTimestamp)
	if err != nil {
		return result, err
	}
	// WIS-515: mask real model names for the user's hidden system tokens so the
	// 费用明细 API never returns a raw model_name (boundary #2). Identified by
	// token_id + hidden, never by model-name string.
	hiddenSet := make(map[int]bool)
	for _, id := range getHiddenSystemTokenIdsForUser(userId) {
		hiddenSet[id] = true
	}

	items := make([]FeatureUsageDetailItem, 0, len(rows))
	for _, r := range rows {
		if filter.FeatureCode == "uncategorized" {
			if common.WischoicerIsKnownFeatureCode(r.FeatureCode) {
				continue
			}
		} else if filter.FeatureCode != "" && r.FeatureCode != filter.FeatureCode {
			continue
		}
		if filter.BizTaskID != "" && r.BizTaskID != filter.BizTaskID {
			continue
		}
		if filter.OperationCode != "" && r.OperationCode != filter.OperationCode {
			continue
		}

		signed := featureUsageSignedQuota(r.Log)
		logType := "consume"
		if r.Log.Type == LogTypeRefund {
			logType = "refund"
		}
		featureName := common.WischoicerFeatureDisplayName(r.FeatureCode)
		if featureName == "" {
			featureName = r.FeatureName
		}

		// request_id / upstream_request_id 来源：request/submit 取顶层列；settle/refund 复用快照。
		var requestID, upstreamID *string
		if r.BillingStage == common.WischoicerStageRequest || r.BillingStage == common.WischoicerStageSubmit {
			requestID = strPtrOrNil(r.Log.RequestId)
			upstreamID = strPtrOrNil(r.Log.UpstreamRequestId)
		} else {
			requestID = strPtrOrNil(r.SnapRequestID)
			upstreamID = strPtrOrNil(r.SnapUpstreamID)
		}

		items = append(items, FeatureUsageDetailItem{
			CreatedAt:         r.Log.CreatedAt,
			FeatureCode:       r.FeatureCode,
			FeatureName:       featureName,
			BizTaskID:         r.BizTaskID,
			BizTaskTitle:      r.BizTaskTitle,
			OperationCode:     r.OperationCode,
			OperationName:     r.OperationName,
			SubTaskID:         r.SubTaskID,
			BillingStage:      r.BillingStage,
			LogType:           logType,
			ModelName:         r.Log.ModelName,
			TokenName:         r.Log.TokenName,
			TokenID:           r.Log.TokenId,
			Quota:             signed,
			CostRMB:           featureUsageCostRMB(signed),
			PromptTokens:      r.Log.PromptTokens,
			CompletionTokens:  r.Log.CompletionTokens,
			RequestID:         requestID,
			UpstreamRequestID: upstreamID,
			ProviderTaskID:    strPtrOrNil(r.ProviderTaskID),
		})
	}
	// WIS-515: rewrite real model_name → system alias for hidden system token rows
	// (covers historical rows written before WIS-505's write-time masking).
	for i := range items {
		if items[i].TokenID != 0 && hiddenSet[items[i].TokenID] {
			items[i].ModelName = common.MaskedSystemModelAlias
		}
	}
	// 按 created_at 倒序，相同则 request_id 倒序，分页稳定。
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt != items[j].CreatedAt {
			return items[i].CreatedAt > items[j].CreatedAt
		}
		return strValOrEmpty(items[i].RequestID) > strValOrEmpty(items[j].RequestID)
	})
	result.Total = len(items)
	start := (page - 1) * pageSize
	if start >= len(items) {
		return result, nil
	}
	end := start + pageSize
	if end > len(items) {
		end = len(items)
	}
	result.Items = items[start:end]
	return result, nil
}

// containsFold 大小写不敏感的子串匹配。
func containsFold(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

// strPtrOrNil 空串→nil（JSON null），非空→*string。
func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// strValOrEmpty 安全取 *string 的值。
func strValOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
