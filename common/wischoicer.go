package common

import (
	"net/http"
	"strings"
)

// Wischoicer 业务归因协议（WIS-499 RFC §2）。
//
// content-workstation → wischoicer-ai-gateway → new-api 全链路通过 X-Wischoicer-* 白名单
// header 承载业务归因，new-api 收敛为 logs.other.wischoicer 嵌套对象，并由 new-api 自身
// 补 schema_version 与 billing_stage。本结构对应 other.wischoicer 中除 billing_stage 外
// 的全部字段；billing_stage 由日志记录阶段（同步/异步）注入，不持久化在本结构里。
//
// 铁律（RFC §2.3）：聚合主键是 biz_task_id + feature_code；feature/operation 的 code 才是
// 事实源，name 仅为展示快照；禁止记录 prompt / 签名 URL / 图片 URL / 敏感输入。
type WischoicerAttribution struct {
	SchemaVersion int    `json:"schema_version"`
	SourceService string `json:"source_service"`
	// InternalFunction=true 才进入功能计费归因；与 SourceService 联合判定是否归因。
	InternalFunction bool   `json:"internal_function"`
	FeatureCode      string `json:"feature_code"`
	FeatureName      string `json:"feature_name,omitempty"`
	OperationCode    string `json:"operation_code"`
	OperationName    string `json:"operation_name,omitempty"`
	BizTaskID        string `json:"biz_task_id"`
	BizTaskTitle     string `json:"biz_task_title,omitempty"`
	SubTaskID        string `json:"sub_task_id,omitempty"`
	// AccountID 落库写归一化后的 effective_account_id = account_id || app_user_id
	// （RFC 回炉 delta：app_user_id 必填，account_id v1 optional，当前 MVP 同值）。
	AccountID string `json:"account_id"`
	AppUserID string `json:"app_user_id"`
}

// WischoicerSchemaVersion 当前 other.wischoicer 结构版本。
const WischoicerSchemaVersion = 1

const (
	WischoicerSourceContentWorkstation = "content-workstation"

	// billing_stage 枚举（new-api 生成，不由上游传）。
	WischoicerStageRequest = "request" // 同步 chat / image 消费日志
	WischoicerStageSubmit  = "submit"  // 异步视频首次提交日志
	WischoicerStageSettle  = "settle"  // 异步视频轮询后的差额补扣日志
	WischoicerStageRefund  = "refund"  // 异步视频失败或回退日志
)

// X-Wischoicer-* header 名（RFC §2.1 白名单）。Go 的 http.Header.Get 会做
// canonical 化，这里保持 canonical 形式以匹配。
const (
	HeaderWischoicerSourceService    = "X-Wischoicer-Source-Service"
	HeaderWischoicerInternalFunction = "X-Wischoicer-Internal-Function"
	HeaderWischoicerFeatureCode      = "X-Wischoicer-Feature-Code"
	HeaderWischoicerFeatureName      = "X-Wischoicer-Feature-Name"
	HeaderWischoicerOperationCode    = "X-Wischoicer-Operation-Code"
	HeaderWischoicerOperationName    = "X-Wischoicer-Operation-Name"
	HeaderWischoicerBizTaskID        = "X-Wischoicer-Biz-Task-Id"
	HeaderWischoicerBizTaskTitle     = "X-Wischoicer-Biz-Task-Title"
	HeaderWischoicerSubTaskID        = "X-Wischoicer-Sub-Task-Id"
	HeaderWischoicerAccountID        = "X-Wischoicer-Account-Id"
	HeaderWischoicerAppUserID        = "X-Wischoicer-App-User-Id"
)

// 已启用的 feature_code（RFC §3.1）。预留枚举也收录，便于未来扩展时不被误判为 uncategorized。
var wischoicerFeatureCodes = map[string]string{
	"reference_copy":       "爆款复刻中心 - 图文爆款复刻",
	"merch_video_clone":    "爆款复刻中心 - 带货视频爆款复刻",
	"image_creation":       "爆款内容工坊 - 图片创作",
	"copywriting_creation": "爆款内容工坊 - 文案创作",
	"video_creation":       "爆款内容工坊 - 视频创作",
}

// WischoicerFeatureDisplayName 返回 feature_code 对应的 canonical 中文展示名；
// 未知 code 返回空串（调用方应回退到日志快照 feature_name）。
func WischoicerFeatureDisplayName(code string) string {
	return wischoicerFeatureCodes[code]
}

// WischoicerOtherFeatureLabel 归因有效但 feature_code 缺失时面向用户的兜底展示名。
// 用于 analytics 汇总，避免把「缺 feature_code / 内部调用」等技术文案暴露给客户。
const WischoicerOtherFeatureLabel = "其他功能消耗"

// WischoicerIsKnownFeatureCode 判断 feature_code 是否属于已定义枚举（含预留）。
// 不在枚举内（含空串）的 code 在聚合时归入 uncategorized。
func WischoicerIsKnownFeatureCode(code string) bool {
	_, ok := wischoicerFeatureCodes[code]
	return ok
}

// WischoicerIsValidBillingStage 判断 billing_stage 是否合法。
func WischoicerIsValidBillingStage(stage string) bool {
	switch stage {
	case WischoicerStageRequest, WischoicerStageSubmit, WischoicerStageSettle, WischoicerStageRefund:
		return true
	}
	return false
}

// ParseWischoicerAttribution 从请求 header 解析业务归因。
//
// 仅当 SourceService=content-workstation 且 InternalFunction=true，且 RFC §2.1 必填键
// biz_task_id / app_user_id 均非空时返回非 nil；否则返回 nil（普通 API Key 调用、或
// partial rollout 缺键的请求都不进入归因统计）。AccountID 写归一化后的
// effective_account_id（account_id 优先，空则 app_user_id）。
func ParseWischoicerAttribution(h http.Header) *WischoicerAttribution {
	if h == nil {
		return nil
	}
	source := strings.TrimSpace(h.Get(HeaderWischoicerSourceService))
	if source != WischoicerSourceContentWorkstation {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(h.Get(HeaderWischoicerInternalFunction)), "true") {
		return nil
	}
	appUserID := strings.TrimSpace(h.Get(HeaderWischoicerAppUserID))
	bizTaskID := strings.TrimSpace(h.Get(HeaderWischoicerBizTaskID))
	// 必填键卡死：缺 biz_task_id 会把同 feature 脏日志折成空任务；缺 app_user_id 会让
	// effective_account_id 归一化失效。二者缺一即不落库、不当有效归因。
	if bizTaskID == "" || appUserID == "" {
		return nil
	}
	accountID := strings.TrimSpace(h.Get(HeaderWischoicerAccountID))
	effectiveAccountID := accountID
	if effectiveAccountID == "" {
		effectiveAccountID = appUserID
	}
	return &WischoicerAttribution{
		SchemaVersion:    WischoicerSchemaVersion,
		SourceService:    WischoicerSourceContentWorkstation,
		InternalFunction: true,
		FeatureCode:      strings.TrimSpace(h.Get(HeaderWischoicerFeatureCode)),
		FeatureName:      strings.TrimSpace(h.Get(HeaderWischoicerFeatureName)),
		OperationCode:    strings.TrimSpace(h.Get(HeaderWischoicerOperationCode)),
		OperationName:    strings.TrimSpace(h.Get(HeaderWischoicerOperationName)),
		BizTaskID:        bizTaskID,
		BizTaskTitle:     strings.TrimSpace(h.Get(HeaderWischoicerBizTaskTitle)),
		SubTaskID:        strings.TrimSpace(h.Get(HeaderWischoicerSubTaskID)),
		AccountID:        effectiveAccountID,
		AppUserID:        appUserID,
	}
}

// IsAttributed 该归因是否进入功能计费统计（source_service=content-workstation && internal_function）。
// 解析阶段已保证这两条，这里作为防御性断言供聚合层复用。
func (a *WischoicerAttribution) IsAttributed() bool {
	return a != nil && a.SourceService == WischoicerSourceContentWorkstation && a.InternalFunction
}

// IsKnownFeature 归因的 feature_code 是否属于已定义枚举。false 表示该条日志应归入 uncategorized。
func (a *WischoicerAttribution) IsKnownFeature() bool {
	return a != nil && WischoicerIsKnownFeatureCode(a.FeatureCode)
}

// WischoicerAttributionFromMap 从已落库的 other.wischoicer 嵌套对象反解析归因。
// 供聚合查询层从 logs.other 读回归因用；与 ToOtherMap 互逆。无 source/internal 标记、
// 或缺 RFC §2.1 必填键 biz_task_id / app_user_id 时返回 nil（剔除 partial rollout 脏数据，
// 避免空 biz_task_id 把同 feature 折成空任务）。
func WischoicerAttributionFromMap(m map[string]interface{}) *WischoicerAttribution {
	if m == nil {
		return nil
	}
	getStr := func(k string) string {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
		return ""
	}
	a := &WischoicerAttribution{
		SchemaVersion: WischoicerSchemaVersion,
		SourceService: getStr("source_service"),
		FeatureCode:   getStr("feature_code"),
		FeatureName:   getStr("feature_name"),
		OperationCode: getStr("operation_code"),
		OperationName: getStr("operation_name"),
		BizTaskID:     getStr("biz_task_id"),
		BizTaskTitle:  getStr("biz_task_title"),
		SubTaskID:     getStr("sub_task_id"),
		AccountID:     getStr("account_id"),
		AppUserID:     getStr("app_user_id"),
	}
	if v, ok := m["internal_function"].(bool); ok {
		a.InternalFunction = v
	}
	if a.SourceService != WischoicerSourceContentWorkstation || !a.InternalFunction {
		return nil
	}
	// 必填键防御性二次校验：与 ParseWischoicerAttribution 对称，剔除历史脏数据。
	if a.BizTaskID == "" || a.AppUserID == "" {
		return nil
	}
	return a
}

// WischoicerMapString 读 other.wischoicer 里的字符串字段（request_id / upstream_request_id
// 等快照复用字段），不存在或非字符串时返回空串。
func WischoicerMapString(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// billingStage 由日志记录阶段传入（request/submit/settle/refund），可为空。
// 调用方负责把它放进 other["wischoicer"]。
func (a *WischoicerAttribution) ToOtherMap(billingStage string) map[string]interface{} {
	if a == nil {
		return nil
	}
	m := map[string]interface{}{
		"schema_version":    a.SchemaVersion,
		"source_service":    a.SourceService,
		"internal_function": a.InternalFunction,
		"feature_code":      a.FeatureCode,
		"operation_code":    a.OperationCode,
		"biz_task_id":       a.BizTaskID,
		"account_id":        a.AccountID,
		"app_user_id":       a.AppUserID,
	}
	if a.FeatureName != "" {
		m["feature_name"] = a.FeatureName
	}
	if a.OperationName != "" {
		m["operation_name"] = a.OperationName
	}
	if a.BizTaskTitle != "" {
		m["biz_task_title"] = a.BizTaskTitle
	}
	if a.SubTaskID != "" {
		m["sub_task_id"] = a.SubTaskID
	}
	if WischoicerIsValidBillingStage(billingStage) {
		m["billing_stage"] = billingStage
	}
	return m
}
