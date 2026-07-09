package model

import (
	"sort"
	"strings"
	"time"

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

// FeatureUsageSummaryFilter summary 接口过滤参数。
// WIS-514 方案 A：与 tasks/details 同口径，新增 biz_task_id / task_keyword / operation_code，
// 使顶部汇总与任务聚合、调用明细三层视图口径一致（加性扩参，零值 = 不过滤，向后兼容）。
type FeatureUsageSummaryFilter struct {
	FeatureCode   string
	BizTaskID     string
	TaskKeyword   string // 匹配 biz_task_title（大小写不敏感包含）
	OperationCode string
}

// GetFeatureUsageSummary 聚合功能板块费用概览（RFC §4.1）。
// filter.FeatureCode 非空时仅返回该功能（且 uncategorized 不适用）；为空时返回全部已知功能 + uncategorized。
func GetFeatureUsageSummary(userId int, startTimestamp, endTimestamp int64, filter FeatureUsageSummaryFilter) (FeatureUsageSummaryResult, error) {
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
		if filter.FeatureCode != "" && r.FeatureCode != filter.FeatureCode {
			continue
		}
		// WIS-514 方案 A：summary 与 tasks/details 同口径，按任务 ID / 操作步骤 / 任务标题关键字过滤。
		if filter.BizTaskID != "" && r.BizTaskID != filter.BizTaskID {
			continue
		}
		if filter.OperationCode != "" && r.OperationCode != filter.OperationCode {
			continue
		}
		if filter.TaskKeyword != "" && !containsFold(r.BizTaskTitle, filter.TaskKeyword) {
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
	CreatedAt     int64  `json:"created_at"`
	FeatureCode   string `json:"feature_code"`
	FeatureName   string `json:"feature_name"`
	BizTaskID     string `json:"biz_task_id"`
	BizTaskTitle  string `json:"biz_task_title"`
	OperationCode string `json:"operation_code"`
	OperationName string `json:"operation_name"`
	SubTaskID     string `json:"sub_task_id"`
	BillingStage  string `json:"billing_stage"`
	LogType       string `json:"log_type"`
	// WIS-514: model_name / request_id / upstream_request_id 仅在 self 接口响应序列化层裁剪
	// （json:"-"），不进客户侧响应。原始日志 / 内部排障 / 归因闭环不受影响；结构体字段保留
	// 供稳定分页排序（RequestID）与单测守护（TestFeatureUsageDetails_*RequestID）。
	ModelName         string  `json:"-"`
	TokenName         string  `json:"token_name"`
	TokenID           int     `json:"token_id"`
	Quota             int     `json:"quota"`
	CostRMB           float64 `json:"cost_rmb"`
	PromptTokens      int     `json:"prompt_tokens"`
	CompletionTokens  int     `json:"completion_tokens"`
	RequestID         *string `json:"-"`
	UpstreamRequestID *string `json:"-"`
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
// WIS-514 方案 A：新增 TaskKeyword，与 tasks 同口径，使调用明细受任务标题关键字筛选。
type FeatureUsageDetailsFilter struct {
	FeatureCode   string
	BizTaskID     string
	TaskKeyword   string // 匹配 biz_task_title（大小写不敏感包含）
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
		if filter.TaskKeyword != "" && !containsFold(r.BizTaskTitle, filter.TaskKeyword) {
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

// WIS-523 §建议后端实现：功能模块消耗分析（user-self 趋势接口）。
//
// 与 summary/tasks/details 的差异：analytics 不按静态 feature 枚举折叠未知 feature。
// 任意非空 feature_code（含尚未进枚举的未来新板块）都独立成行，按日志快照/兜底名展示；
// 仅 feature_code 缺失的归因日志归入「其他功能消耗」。时间桶在 Go 层按 UTC 对齐
// （与 quota_data 的小时桶口径一致），bucket_ts 为 Unix 秒，前端按本地时区格式化。

const (
	FeatureUsageGranularityHour = "hour"
	FeatureUsageGranularityDay  = "day"
	FeatureUsageGranularityWeek = "week"
)

// featureUsageTokenUsed token 计入口径：仅 consume 行计入 prompt+completion，
// refund 不倒扣 token（WIS-523 §后端统计口径 5）。
func featureUsageTokenUsed(lg *Log) int {
	if lg.Type == LogTypeRefund {
		return 0
	}
	return lg.PromptTokens + lg.CompletionTokens
}

// featureUsageFeatureName 面向用户的功能展示名。
// 空 feature_code → 「其他功能消耗」；非空 code → 枚举 canonical 名优先，
// 回退日志快照 feature_name，再回退 code 本身（保证永不为空，未知新板块也能展示）。
func featureUsageFeatureName(code, snapshot string) string {
	if code == "" {
		return common.WischoicerOtherFeatureLabel
	}
	if name := common.WischoicerFeatureDisplayName(code); name != "" {
		return name
	}
	if snapshot != "" {
		return snapshot
	}
	return code
}

// featureUsageNormalizeGranularity 归一化粒度：空串或非法值按时间范围自动推断。
func featureUsageNormalizeGranularity(granularity string, spanSeconds int64) string {
	switch granularity {
	case FeatureUsageGranularityHour, FeatureUsageGranularityDay, FeatureUsageGranularityWeek:
		return granularity
	}
	// 自动推断：≤3 天按小时、≤84 天（12 周）按日、否则按周。
	switch {
	case spanSeconds <= 3*24*60*60:
		return FeatureUsageGranularityHour
	case spanSeconds <= 84*24*60*60:
		return FeatureUsageGranularityDay
	default:
		return FeatureUsageGranularityWeek
	}
}

// featureUsageBucketTs 把日志时间归到 UTC 桶起点。hour/day 用整数取模（与 quota_data 一致）；
// week 以周一 00:00 UTC 为桶起点（Go time 计算星期，不依赖 epoch 对齐）。
func featureUsageBucketTs(ts int64, granularity string) int64 {
	switch granularity {
	case FeatureUsageGranularityHour:
		return ts - ts%3600
	case FeatureUsageGranularityWeek:
		t := time.Unix(ts, 0).UTC()
		daysSinceMonday := (int(t.Weekday()) + 6) % 7 // 周一→0 … 周日→6
		midnight := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		return midnight.AddDate(0, 0, -daysSinceMonday).Unix()
	default: // day
		return ts - ts%86400
	}
}

// featureUsageBucketLabel 桶展示标签。hour 带 HH:mm；day/week 取桶起点的 MM-DD。
func featureUsageBucketLabel(bucketTs int64, granularity string) string {
	t := time.Unix(bucketTs, 0).UTC()
	if granularity == FeatureUsageGranularityHour {
		return t.Format("01-02 15:04")
	}
	return t.Format("01-02")
}

// FeatureUsageAnalyticsTotals analytics 顶部合计。
type FeatureUsageAnalyticsTotals struct {
	Quota        int     `json:"quota"`
	CostRMB      float64 `json:"cost_rmb"`
	RequestCount int     `json:"request_count"`
	TokenUsed    int     `json:"token_used"`
	AvgRPM       float64 `json:"avg_rpm"`
	AvgTPM       float64 `json:"avg_tpm"`
}

// FeatureUsageAnalyticsFeature analytics 按功能模块聚合行。
type FeatureUsageAnalyticsFeature struct {
	FeatureCode  string  `json:"feature_code"`
	FeatureName  string  `json:"feature_name"`
	Quota        int     `json:"quota"`
	CostRMB      float64 `json:"cost_rmb"`
	RequestCount int     `json:"request_count"`
	TokenUsed    int     `json:"token_used"`
	FirstSeen    int64   `json:"first_seen"`
	LastSeen     int64   `json:"last_seen"`
}

// FeatureUsageAnalyticsPoint analytics 时间桶 × 功能模块聚合行（堆叠图单点）。
type FeatureUsageAnalyticsPoint struct {
	BucketTs     int64   `json:"bucket_ts"`
	BucketLabel  string  `json:"bucket_label"`
	FeatureCode  string  `json:"feature_code"`
	FeatureName  string  `json:"feature_name"`
	Quota        int     `json:"quota"`
	CostRMB      float64 `json:"cost_rmb"`
	RequestCount int     `json:"request_count"`
	TokenUsed    int     `json:"token_used"`
}

// FeatureUsageAnalyticsResult analytics 接口响应 data。
// Granularity 为实际生效的聚合粒度（自动推断时回填），前端据此格式化坐标轴/图例。
type FeatureUsageAnalyticsResult struct {
	Totals      FeatureUsageAnalyticsTotals    `json:"totals"`
	Features    []FeatureUsageAnalyticsFeature `json:"features"`
	Points      []FeatureUsageAnalyticsPoint   `json:"points"`
	Granularity string                         `json:"granularity"`
}

// FeatureUsageAnalyticsFilter analytics 接口过滤参数（与 summary 同口径，加性扩参）。
// FeatureCode 为普通字符串匹配：不按静态枚举校验，动态/未来 feature 也能查。
type FeatureUsageAnalyticsFilter struct {
	FeatureCode   string
	BizTaskID     string
	TaskKeyword   string // 匹配 biz_task_title（大小写不敏感包含）
	OperationCode string
}

// GetFeatureUsageAnalytics 功能模块消耗趋势聚合（WIS-523 §建议后端实现）。
// 口径：复用 loadSelfAttributedLogs；quota/cost_rmb 净额（consume 正 / refund 负）；
// request_count 只计 request/submit；token_used 仅 consume 行计入；avg_rpm/avg_tpm =
// request_count|token_used / 时间范围分钟数；未知非空 feature 自动成行，缺 feature_code 归「其他功能消耗」。
func GetFeatureUsageAnalytics(userId int, startTimestamp, endTimestamp int64, granularity string, filter FeatureUsageAnalyticsFilter) (FeatureUsageAnalyticsResult, error) {
	result := FeatureUsageAnalyticsResult{
		Features: []FeatureUsageAnalyticsFeature{},
		Points:   []FeatureUsageAnalyticsPoint{},
	}
	rows, err := loadSelfAttributedLogs(userId, startTimestamp, endTimestamp)
	if err != nil {
		return result, err
	}
	granularity = featureUsageNormalizeGranularity(granularity, endTimestamp-startTimestamp)
	result.Granularity = granularity

	type featAcc struct {
		name         string
		requestCount int
		quota        int
		tokenUsed    int
		first        int64
		last         int64
	}
	feats := make(map[string]*featAcc)

	type pointKey struct {
		bucket int64
		code   string
	}
	type pointAcc struct {
		name         string
		requestCount int
		quota        int
		tokenUsed    int
	}
	points := make(map[pointKey]*pointAcc)

	var totalsQuota, totalsRequest, totalsToken int
	for _, r := range rows {
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

		signed := featureUsageSignedQuota(r.Log)
		tok := featureUsageTokenUsed(r.Log)
		countsAsReq := featureUsageCountsAsRequest(r.BillingStage)
		seen := r.Log.CreatedAt
		name := featureUsageFeatureName(r.FeatureCode, r.FeatureName)

		totalsQuota += signed
		totalsToken += tok
		if countsAsReq {
			totalsRequest++
		}

		acc, ok := feats[r.FeatureCode]
		if !ok {
			acc = &featAcc{name: name}
			feats[r.FeatureCode] = acc
		}
		if acc.name == "" {
			acc.name = name
		}
		acc.quota += signed
		acc.tokenUsed += tok
		if countsAsReq {
			acc.requestCount++
		}
		if acc.first == 0 || seen < acc.first {
			acc.first = seen
		}
		if seen > acc.last {
			acc.last = seen
		}

		bucket := featureUsageBucketTs(seen, granularity)
		pk := pointKey{bucket: bucket, code: r.FeatureCode}
		pa, ok := points[pk]
		if !ok {
			pa = &pointAcc{name: name}
			points[pk] = pa
		}
		if pa.name == "" {
			pa.name = name
		}
		pa.quota += signed
		pa.tokenUsed += tok
		if countsAsReq {
			pa.requestCount++
		}
	}

	minutes := float64(endTimestamp-startTimestamp) / 60.0
	result.Totals = FeatureUsageAnalyticsTotals{
		Quota:        totalsQuota,
		CostRMB:      featureUsageCostRMB(totalsQuota),
		RequestCount: totalsRequest,
		TokenUsed:    totalsToken,
	}
	if minutes > 0 {
		result.Totals.AvgRPM = float64(totalsRequest) / minutes
		result.Totals.AvgTPM = float64(totalsToken) / minutes
	}

	for code, acc := range feats {
		result.Features = append(result.Features, FeatureUsageAnalyticsFeature{
			FeatureCode:  code,
			FeatureName:  acc.name,
			Quota:        acc.quota,
			CostRMB:      featureUsageCostRMB(acc.quota),
			RequestCount: acc.requestCount,
			TokenUsed:    acc.tokenUsed,
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

	for pk, pa := range points {
		result.Points = append(result.Points, FeatureUsageAnalyticsPoint{
			BucketTs:     pk.bucket,
			BucketLabel:  featureUsageBucketLabel(pk.bucket, granularity),
			FeatureCode:  pk.code,
			FeatureName:  pa.name,
			Quota:        pa.quota,
			CostRMB:      featureUsageCostRMB(pa.quota),
			RequestCount: pa.requestCount,
			TokenUsed:    pa.tokenUsed,
		})
	}
	sort.Slice(result.Points, func(i, j int) bool {
		if result.Points[i].BucketTs != result.Points[j].BucketTs {
			return result.Points[i].BucketTs < result.Points[j].BucketTs
		}
		if result.Points[i].Quota != result.Points[j].Quota {
			return result.Points[i].Quota > result.Points[j].Quota
		}
		return result.Points[i].FeatureCode < result.Points[j].FeatureCode
	})

	return result, nil
}
