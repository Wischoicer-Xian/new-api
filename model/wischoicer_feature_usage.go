package model

import (
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// ---------------------------------------------------------------------------
// Wischoicer 费用明细查询（WIS-499 RFC §4）
//
// 三个 user-self 接口的聚合逻辑。v1 不做 schema migration / 新表 / 新索引，基于
// 现有 logs.other JSON 文本字段在应用侧解析聚合（RFC §7 R3 升级条件：30 天窗口
// p95 不可接受 → 报告升级，不自行加索引）。兼容 SQLite / MySQL / PostgreSQL，
// 不走任一家专属 JSON 索引方案。
// ---------------------------------------------------------------------------

// 查询窗口上限（RFC §4 公共规则：后端拒绝 > 92 天的单次查询窗口）。
const WischoicerFeatureUsageMaxWindowDays = 92

var (
	errWischoicerTimeRequired  = errors.New("start_timestamp 与 end_timestamp 为必填项")
	errWischoicerTimeRange     = errors.New("start_timestamp 不能大于 end_timestamp")
	errWischoicerWindowTooWide = errors.New("单次查询窗口不能超过 92 天")
)

// wischoicerLogRow 是 feature-usage 查询的原始日志行（仅取需要的列，避免加载 content）。
type wischoicerLogRow struct {
	Id                int64  `json:"id" gorm:"column:id"`
	CreatedAt         int64  `json:"created_at" gorm:"column:created_at"`
	Type              int    `json:"type" gorm:"column:type"`
	ModelName         string `json:"model_name" gorm:"column:model_name"`
	TokenName         string `json:"token_name" gorm:"column:token_name"`
	TokenId           int    `json:"token_id" gorm:"column:token_id"`
	Quota             int    `json:"quota" gorm:"column:quota"`
	PromptTokens      int    `json:"prompt_tokens" gorm:"column:prompt_tokens"`
	CompletionTokens  int    `json:"completion_tokens" gorm:"column:completion_tokens"`
	RequestId         string `json:"request_id" gorm:"column:request_id"`
	UpstreamRequestId string `json:"upstream_request_id" gorm:"column:upstream_request_id"`
	Other             string `json:"other" gorm:"column:other"`
}

// loadWischoicerLogRows 加载指定用户在时间窗口内全部 consume/refund 日志行。
// 仅 user-self + 时间窗口过滤下沉到 DB；wischoicer 字段过滤在应用侧完成（Other 为
// JSON 文本，跨库无统一 JSON 索引方案）。order 对齐现有 GetUserLogs 的排序习惯。
func loadWischoicerLogRows(userId int, startTs, endTs int64) ([]wischoicerLogRow, error) {
	var rows []wischoicerLogRow
	tx := LOG_DB.Model(&Log{}).
		Select("id, created_at, type, model_name, token_name, token_id, quota, prompt_tokens, completion_tokens, request_id, upstream_request_id, other").
		Where("user_id = ?", userId).
		Where("type IN ?", []int{LogTypeConsume, LogTypeRefund}).
		Where("created_at >= ?", startTs).
		Where("created_at <= ?", endTs)
	order := "created_at desc, id desc"
	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		order = clickHouseLogOrder("")
	}
	err := tx.Order(order).Find(&rows).Error
	return rows, err
}

// wischoicerParsedRow 是一行日志解析后的归因视图。
type wischoicerParsedRow struct {
	row    wischoicerLogRow
	wisc   *WischoicerAttribution
	stage  string // billing_stage，来自 other.wischoicer.billing_stage
	// otherMap 保留原始 other map，用于读取 request_id / upstream_request_id / task_id
	// 等 settle/refund 快照字段（不在 wischoicer 子对象内）。
	otherMap      map[string]interface{}
	qualifies     bool // 是否命中 source_service=content-workstation && internal_function=true
	uncategorized bool // 是否归入 uncategorized（feature_code 缺失/未知）
}

// parseWischoicerRow 解析一行日志的 Other JSON，提取 wischoicer 归因与 stage。
func parseWischoicerRow(row wischoicerLogRow) wischoicerParsedRow {
	pr := wischoicerParsedRow{row: row}
	if strings.TrimSpace(row.Other) == "" {
		return pr
	}
	otherMap, err := common.StrToMap(row.Other)
	if err != nil || otherMap == nil {
		return pr
	}
	pr.otherMap = otherMap
	wMap, _ := otherMap["wischoicer"].(map[string]interface{})
	if wMap == nil {
		return pr
	}
	w := WischoicerAttributionFromMap(wMap)
	if w == nil {
		return pr
	}
	pr.wisc = w
	pr.stage = stringFromAny(wMap["billing_stage"])
	// 仅 source_service=content-workstation && internal_function=true 进入归因统计
	if w.SourceService == WischoicerSourceServiceContentWorkstation && w.InternalFunction {
		pr.qualifies = true
		if !IsKnownWischoicerFeatureCode(w.FeatureCode) {
			pr.uncategorized = true
		}
	}
	return pr
}

// signedQuota 返回带正负号的净值 quota（consume 正、refund 负，RFC §4）。
func (pr wischoicerParsedRow) signedQuota() int {
	if pr.row.Type == LogTypeRefund {
		return -pr.row.Quota
	}
	return pr.row.Quota
}

// countsAsRequest 判断该行是否计入 request_count（仅 billing_stage in request/submit）。
func (pr wischoicerParsedRow) countsAsRequest() bool {
	switch pr.stage {
	case BillingStageRequest, BillingStageSubmit:
		return true
	}
	return false
}

// linkRequestID 返回 details 接口所需的 request_id。
// request/submit 阶段取日志顶层列；settle/refund 阶段取 other.request_id 快照
// （回炉收敛 b9996b28：无快照返回空，由调用方转为 null）。
func (pr wischoicerParsedRow) linkRequestID() string {
	if pr.stage == BillingStageRequest || pr.stage == BillingStageSubmit {
		return pr.row.RequestId
	}
	return stringFromAny(pr.otherMap["request_id"])
}

func (pr wischoicerParsedRow) linkUpstreamRequestID() string {
	if pr.stage == BillingStageRequest || pr.stage == BillingStageSubmit {
		return pr.row.UpstreamRequestId
	}
	return stringFromAny(pr.otherMap["upstream_request_id"])
}

// providerTaskID 返回 details 接口所需的 provider_task_id（other.task_id，有则返）。
func (pr wischoicerParsedRow) providerTaskID() string {
	return stringFromAny(pr.otherMap["task_id"])
}

// costRmbFromQuota 按 RFC §4：cost_rmb = quota / 500000（QuotaPerUnit）。
func costRmbFromQuota(quota int) float64 {
	return float64(quota) / common.QuotaPerUnit
}

// FeatureUsageTotals 汇总统计。
type FeatureUsageTotals struct {
	Quota        int     `json:"quota"`
	CostRmb      float64 `json:"cost_rmb"`
	RequestCount int     `json:"request_count"`
}

// FeatureUsageSummaryItem 单个 feature 的汇总条目。
type FeatureUsageSummaryItem struct {
	FeatureCode  string  `json:"feature_code"`
	FeatureName  string  `json:"feature_name"`
	TaskCount    int     `json:"task_count"`
	RequestCount int     `json:"request_count"`
	Quota        int     `json:"quota"`
	CostRmb      float64 `json:"cost_rmb"`
	FirstSeen    int64   `json:"first_seen"`
	LastSeen     int64   `json:"last_seen"`
}

// FeatureUsageUncategorized 未命中 feature_code 的归因日志汇总。
type FeatureUsageUncategorized struct {
	Present      bool    `json:"present"`
	Quota        int     `json:"quota"`
	CostRmb      float64 `json:"cost_rmb"`
	RequestCount int     `json:"request_count"`
}

// FeatureUsageSummary summary 接口响应数据。
type FeatureUsageSummary struct {
	Totals        FeatureUsageTotals        `json:"totals"`
	Features      []FeatureUsageSummaryItem `json:"features"`
	Uncategorized FeatureUsageUncategorized `json:"uncategorized"`
}

// FeatureUsageTasksQuery Tasks 接口查询参数。
type FeatureUsageTasksQuery struct {
	UserId        int
	StartTs       int64
	EndTs         int64
	FeatureCode   string // 选填，仅允许已知 feature_code（不允许 uncategorized）
	BizTaskId     string // 选填，精确匹配
	TaskKeyword   string // 选填，匹配 biz_task_title（子串）
	OperationCode string // 选填
	Page          int
	PageSize      int
}

// FeatureUsageTaskItem 单个业务任务聚合条目。
type FeatureUsageTaskItem struct {
	BizTaskId    string  `json:"biz_task_id"`
	BizTaskTitle string  `json:"biz_task_title"`
	FeatureCode  string  `json:"feature_code"`
	FeatureName  string  `json:"feature_name"`
	RequestCount int     `json:"request_count"`
	Quota        int     `json:"quota"`
	CostRmb      float64 `json:"cost_rmb"`
	FirstSeen    int64   `json:"first_seen"`
	LastSeen     int64   `json:"last_seen"`
}

// FeatureUsageTasksResult Tasks 接口响应数据。
type FeatureUsageTasksResult struct {
	Page     int                  `json:"page"`
	PageSize int                  `json:"page_size"`
	Total    int                  `json:"total"`
	Items    []FeatureUsageTaskItem `json:"items"`
}

// FeatureUsageDetailsQuery Details 接口查询参数。
type FeatureUsageDetailsQuery struct {
	UserId        int
	StartTs       int64
	EndTs         int64
	FeatureCode   string // 选填，允许传 uncategorized
	BizTaskId     string // 选填
	OperationCode string // 选填
	Page          int
	PageSize      int
}

// FeatureUsageDetailItem 单条明细。
type FeatureUsageDetailItem struct {
	CreatedAt         int64   `json:"created_at"`
	FeatureCode       string  `json:"feature_code"`
	FeatureName       string  `json:"feature_name"`
	BizTaskId         string  `json:"biz_task_id"`
	BizTaskTitle      string  `json:"biz_task_title"`
	OperationCode     string  `json:"operation_code"`
	OperationName     string  `json:"operation_name"`
	SubTaskId         string  `json:"sub_task_id"`
	BillingStage      string  `json:"billing_stage"`
	LogType           string  `json:"log_type"`
	ModelName         string  `json:"model_name"`
	TokenName         string  `json:"token_name"`
	TokenId           int     `json:"token_id"`
	Quota             int     `json:"quota"`
	CostRmb           float64 `json:"cost_rmb"`
	PromptTokens      int     `json:"prompt_tokens"`
	CompletionTokens  int     `json:"completion_tokens"`
	RequestId         *string `json:"request_id"`
	UpstreamRequestId *string `json:"upstream_request_id"`
	ProviderTaskId    *string `json:"provider_task_id"`
}

// FeatureUsageDetailsResult Details 接口响应数据。
type FeatureUsageDetailsResult struct {
	Page     int                   `json:"page"`
	PageSize int                   `json:"page_size"`
	Total    int                   `json:"total"`
	Items    []FeatureUsageDetailItem `json:"items"`
}

// strPtr 把非空字符串转为 *string，空串返回 nil（nullable 字段）。
func strPtr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// featureDisplayName 优先用 canonical code map，缺失时回退到日志快照 name。
func featureDisplayName(code, snapshot string) string {
	if name := WischoicerFeatureDisplayName(code); name != "" {
		return name
	}
	return snapshot
}

// GetFeatureUsageSummary 聚合 summary 接口数据。
func GetFeatureUsageSummary(userId int, startTs, endTs int64, featureCode string) (*FeatureUsageSummary, error) {
	rows, err := loadWischoicerLogRows(userId, startTs, endTs)
	if err != nil {
		return nil, err
	}
	// featureCode 过滤：仅允许已知 feature code；若传入未知 code（含 uncategorized），
	// 视为无匹配（RFC §4.1：不允许 uncategorized）。
	featureFilter := ""
	if featureCode != "" {
		if !IsKnownWischoicerFeatureCode(featureCode) {
			return &FeatureUsageSummary{
				Totals:   FeatureUsageTotals{},
				Features: []FeatureUsageSummaryItem{},
				Uncategorized: FeatureUsageUncategorized{},
			}, nil
		}
		featureFilter = featureCode
	}

	type featAgg struct {
		quota        int
		requestCount int
		tasks        map[string]struct{}
		firstSeen    int64
		lastSeen     int64
		nameSnapshot string
	}
	feats := map[string]*featAgg{}
	var unc FeatureUsageUncategorized
	var totals FeatureUsageTotals

	for _, row := range rows {
		pr := parseWischoicerRow(row)
		if !pr.qualifies {
			continue
		}
		if featureFilter != "" && pr.wisc.FeatureCode != featureFilter {
			// 指定 feature 时，uncategorized 行与其它 feature 行都不计入
			continue
		}
		sq := pr.signedQuota()
		totals.Quota += sq
		if pr.countsAsRequest() {
			totals.RequestCount++
		}

		if pr.uncategorized {
			// 指定 feature 时不进入 uncategorized（featureFilter 已过滤）
			unc.Present = true
			unc.Quota += sq
			if pr.countsAsRequest() {
				unc.RequestCount++
			}
			continue
		}

		agg, ok := feats[pr.wisc.FeatureCode]
		if !ok {
			agg = &featAgg{tasks: map[string]struct{}{}, firstSeen: row.CreatedAt, lastSeen: row.CreatedAt}
			feats[pr.wisc.FeatureCode] = agg
		}
		agg.quota += sq
		if pr.countsAsRequest() {
			agg.requestCount++
		}
		if pr.wisc.BizTaskId != "" {
			agg.tasks[pr.wisc.BizTaskId] = struct{}{}
		}
		if row.CreatedAt < agg.firstSeen {
			agg.firstSeen = row.CreatedAt
		}
		if row.CreatedAt > agg.lastSeen {
			agg.lastSeen = row.CreatedAt
		}
		if agg.nameSnapshot == "" {
			agg.nameSnapshot = pr.wisc.FeatureName
		}
	}
	totals.CostRmb = costRmbFromQuota(totals.Quota)
	unc.CostRmb = costRmbFromQuota(unc.Quota)

	features := make([]FeatureUsageSummaryItem, 0, len(feats))
	for code, agg := range feats {
		features = append(features, FeatureUsageSummaryItem{
			FeatureCode:  code,
			FeatureName:  featureDisplayName(code, agg.nameSnapshot),
			TaskCount:    len(agg.tasks),
			RequestCount: agg.requestCount,
			Quota:        agg.quota,
			CostRmb:      costRmbFromQuota(agg.quota),
			FirstSeen:    agg.firstSeen,
			LastSeen:     agg.lastSeen,
		})
	}
	// 按 last_seen desc 排序，便于 UI 展示最近活跃功能在前。
	sort.Slice(features, func(i, j int) bool {
		if features[i].LastSeen != features[j].LastSeen {
			return features[i].LastSeen > features[j].LastSeen
		}
		return features[i].FeatureCode < features[j].FeatureCode
	})

	return &FeatureUsageSummary{
		Totals:        totals,
		Features:      features,
		Uncategorized: unc,
	}, nil
}

// GetFeatureUsageTasks 聚合 tasks 接口数据。
func GetFeatureUsageTasks(q FeatureUsageTasksQuery) (*FeatureUsageTasksResult, error) {
	rows, err := loadWischoicerLogRows(q.UserId, q.StartTs, q.EndTs)
	if err != nil {
		return nil, err
	}

	type taskAgg struct {
		bizTaskId    string
		featureCode  string
		featureName  string
		taskTitle    string
		quota        int
		requestCount int
		firstSeen    int64
		lastSeen     int64
	}
	tasks := map[string]*taskAgg{} // key = feature_code + "\x00" + biz_task_id

	for _, row := range rows {
		pr := parseWischoicerRow(row)
		if !pr.qualifies || pr.uncategorized {
			// tasks 不支持 uncategorized（RFC §4.2）
			continue
		}
		// 行级过滤
		if q.FeatureCode != "" && pr.wisc.FeatureCode != q.FeatureCode {
			continue
		}
		if q.BizTaskId != "" && pr.wisc.BizTaskId != q.BizTaskId {
			continue
		}
		if q.OperationCode != "" && pr.wisc.OperationCode != q.OperationCode {
			continue
		}
		key := pr.wisc.FeatureCode + "\x00" + pr.wisc.BizTaskId
		agg, ok := tasks[key]
		if !ok {
			agg = &taskAgg{
				bizTaskId:   pr.wisc.BizTaskId,
				featureCode: pr.wisc.FeatureCode,
				featureName: pr.wisc.FeatureName,
				taskTitle:   pr.wisc.BizTaskTitle,
				firstSeen:   row.CreatedAt,
				lastSeen:    row.CreatedAt,
			}
			tasks[key] = agg
		}
		agg.quota += pr.signedQuota()
		if pr.countsAsRequest() {
			agg.requestCount++
		}
		if row.CreatedAt < agg.firstSeen {
			agg.firstSeen = row.CreatedAt
		}
		if row.CreatedAt > agg.lastSeen {
			agg.lastSeen = row.CreatedAt
		}
		// 取较新行的 title/name 快照（标题可能在任务生命周期内被更新）
		if pr.wisc.BizTaskTitle != "" {
			agg.taskTitle = pr.wisc.BizTaskTitle
		}
		if pr.wisc.FeatureName != "" {
			agg.featureName = pr.wisc.FeatureName
		}
	}

	items := make([]FeatureUsageTaskItem, 0, len(tasks))
	for _, agg := range tasks {
		// task_keyword 在聚合后过滤（任务级匹配 biz_task_title）
		if q.TaskKeyword != "" && !strings.Contains(agg.taskTitle, q.TaskKeyword) {
			continue
		}
		items = append(items, FeatureUsageTaskItem{
			BizTaskId:    agg.bizTaskId,
			BizTaskTitle: agg.taskTitle,
			FeatureCode:  agg.featureCode,
			FeatureName:  featureDisplayName(agg.featureCode, agg.featureName),
			RequestCount: agg.requestCount,
			Quota:        agg.quota,
			CostRmb:      costRmbFromQuota(agg.quota),
			FirstSeen:    agg.firstSeen,
			LastSeen:     agg.lastSeen,
		})
	}
	// 按 last_seen desc 排序，便于 UI 展示最近活跃任务在前。
	sort.Slice(items, func(i, j int) bool {
		if items[i].LastSeen != items[j].LastSeen {
			return items[i].LastSeen > items[j].LastSeen
		}
		return items[i].BizTaskId < items[j].BizTaskId
	})

	total := len(items)
	start, end := paginateBounds(total, q.Page, q.PageSize)
	paged := items[start:end]
	if paged == nil {
		paged = []FeatureUsageTaskItem{}
	}
	return &FeatureUsageTasksResult{
		Page:     q.Page,
		PageSize: q.PageSize,
		Total:    total,
		Items:    paged,
	}, nil
}

// paginateBounds 返回 [start, end) 闭开区间的分页切片边界。
func paginateBounds(total, page, pageSize int) (int, int) {
	if pageSize <= 0 {
		pageSize = 20
	}
	if page <= 0 {
		page = 1
	}
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	return start, end
}

// GetFeatureUsageDetails 聚合 details 接口数据。
func GetFeatureUsageDetails(q FeatureUsageDetailsQuery) (*FeatureUsageDetailsResult, error) {
	rows, err := loadWischoicerLogRows(q.UserId, q.StartTs, q.EndTs)
	if err != nil {
		return nil, err
	}
	uncategorizedFilter := q.FeatureCode == "uncategorized"

	items := make([]FeatureUsageDetailItem, 0, len(rows))
	for _, row := range rows {
		pr := parseWischoicerRow(row)
		if !pr.qualifies {
			continue
		}
		if uncategorizedFilter {
			// 仅返回 uncategorized 行
			if !pr.uncategorized {
				continue
			}
		} else {
			// 非 uncategorized 模式：uncategorized 行一律排除（details 默认只看真实 feature）
			if pr.uncategorized {
				continue
			}
			if q.FeatureCode != "" && pr.wisc.FeatureCode != q.FeatureCode {
				continue
			}
		}
		if q.BizTaskId != "" && pr.wisc.BizTaskId != q.BizTaskId {
			continue
		}
		if q.OperationCode != "" && pr.wisc.OperationCode != q.OperationCode {
			continue
		}

		logType := "consume"
		if row.Type == LogTypeRefund {
			logType = "refund"
		}
		items = append(items, FeatureUsageDetailItem{
			CreatedAt:         row.CreatedAt,
			FeatureCode:       pr.wisc.FeatureCode,
			FeatureName:       featureDisplayName(pr.wisc.FeatureCode, pr.wisc.FeatureName),
			BizTaskId:         pr.wisc.BizTaskId,
			BizTaskTitle:      pr.wisc.BizTaskTitle,
			OperationCode:     pr.wisc.OperationCode,
			OperationName:     pr.wisc.OperationName,
			SubTaskId:         pr.wisc.SubTaskId,
			BillingStage:      pr.stage,
			LogType:           logType,
			ModelName:         row.ModelName,
			TokenName:         row.TokenName,
			TokenId:           row.TokenId,
			Quota:             pr.signedQuota(),
			CostRmb:           costRmbFromQuota(pr.signedQuota()),
			PromptTokens:      row.PromptTokens,
			CompletionTokens:  row.CompletionTokens,
			RequestId:         strPtr(pr.linkRequestID()),
			UpstreamRequestId: strPtr(pr.linkUpstreamRequestID()),
			ProviderTaskId:    strPtr(pr.providerTaskID()),
		})
	}

	total := len(items)
	start, end := paginateBounds(total, q.Page, q.PageSize)
	paged := items[start:end]
	if paged == nil {
		paged = []FeatureUsageDetailItem{}
	}
	return &FeatureUsageDetailsResult{
		Page:     q.Page,
		PageSize: q.PageSize,
		Total:    total,
		Items:    paged,
	}, nil
}

// ValidateWischoicerFeatureUsageWindow 校验时间窗口：必填 + 闭区间 + <= 92 天。
func ValidateWischoicerFeatureUsageWindow(startTs, endTs int64) error {
	if startTs <= 0 || endTs <= 0 {
		return errWischoicerTimeRequired
	}
	if startTs > endTs {
		return errWischoicerTimeRange
	}
	dur := time.Duration(endTs-startTs) * time.Second
	if dur > WischoicerFeatureUsageMaxWindowDays*24*time.Hour {
		return errWischoicerWindowTooWide
	}
	return nil
}
