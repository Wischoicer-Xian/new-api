package model

import (
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
)

// ---------------------------------------------------------------------------
// Wischoicer 业务归因协议（WIS-499 RFC 终稿 §2）
//
// 入站 X-Wischoicer-* header 由 content-workstation 打标、ai-gateway 白名单透传，
// new-api 解析后收敛为 logs.other.wischoicer 嵌套对象。billing_stage 与
// schema_version 由 new-api 自身补齐，不由上游传。
//
// 契约唯一源：WIS-499 评论 311d9cbe（RFC 终稿）+ b9996b28（回炉收敛）。
// header 名 / 枚举 / other.wischoicer 结构逐字对齐，不得新增别名。
// ---------------------------------------------------------------------------

// X-Wischoicer-* header 白名单（与 WIS-501 ai-gateway 透传清单逐字一致）。
const (
	WischoicerHeaderSourceService     = "X-Wischoicer-Source-Service"
	WischoicerHeaderInternalFunction  = "X-Wischoicer-Internal-Function"
	WischoicerHeaderFeatureCode       = "X-Wischoicer-Feature-Code"
	WischoicerHeaderOperationCode     = "X-Wischoicer-Operation-Code"
	WischoicerHeaderBizTaskId         = "X-Wischoicer-Biz-Task-Id"
	WischoicerHeaderAccountId         = "X-Wischoicer-Account-Id"
	WischoicerHeaderAppUserId         = "X-Wischoicer-App-User-Id"
	WischoicerHeaderFeatureName       = "X-Wischoicer-Feature-Name"
	WischoicerHeaderOperationName     = "X-Wischoicer-Operation-Name"
	WischoicerHeaderBizTaskTitle      = "X-Wischoicer-Biz-Task-Title"
	WischoicerHeaderSubTaskId         = "X-Wischoicer-Sub-Task-Id"
)

// billing_stage 由 new-api 生成，标记该条归因日志处于计费链路的哪个阶段。
const (
	BillingStageRequest = "request" // 同步 chat / image 消费日志
	BillingStageSubmit  = "submit"  // 异步视频首次提交日志
	BillingStageSettle  = "settle"  // 异步视频轮询后的差额补扣日志
	BillingStageRefund  = "refund"  // 异步视频失败或回退日志
)

// WischoicerSourceServiceContentWorkstation 当前唯一合法的 source_service 值。
const WischoicerSourceServiceContentWorkstation = "content-workstation"

// wischoicerSchemaVersion 当前 other.wischoicer 结构版本号，由 new-api 补齐。
const wischoicerSchemaVersion = 1

// feature_code 枚举（RFC §3.1）。reference_copy / merch_video_clone / image_creation
// 本期启用；copywriting_creation / video_creation 预留。
const (
	FeatureCodeReferenceCopy       = "reference_copy"
	FeatureCodeMerchVideoClone     = "merch_video_clone"
	FeatureCodeImageCreation       = "image_creation"
	FeatureCodeCopywritingCreation = "copywriting_creation"
	FeatureCodeVideoCreation       = "video_creation"
)

// wischoicerFeatureDisplayNames 是 new-api 内的 canonical code → 中文展示名映射。
// 查询聚合时优先用此 map；日志快照 feature_name 仅作回退（RFC §5）。
var wischoicerFeatureDisplayNames = map[string]string{
	FeatureCodeReferenceCopy:       "爆款复刻中心 - 图文爆款复刻",
	FeatureCodeMerchVideoClone:     "爆款复刻中心 - 带货视频爆款复刻",
	FeatureCodeImageCreation:       "爆款内容工坊 - 图片创作",
	FeatureCodeCopywritingCreation: "爆款内容工坊 - 文案创作",
	FeatureCodeVideoCreation:       "爆款内容工坊 - 视频创作",
}

// IsKnownWischoicerFeatureCode 判断 feature_code 是否属于已锁枚举。
// 未命中枚举（含空串）的 content-workstation 内部调用日志归入 uncategorized。
func IsKnownWischoicerFeatureCode(code string) bool {
	_, ok := wischoicerFeatureDisplayNames[code]
	return ok
}

// WischoicerFeatureDisplayName 返回 feature_code 对应的中文展示名；未知 code 返回空串。
func WischoicerFeatureDisplayName(code string) string {
	return wischoicerFeatureDisplayNames[code]
}

// WischoicerAttribution 描述一条业务归因快照（不含 billing_stage，stage 由落库点补）。
// 该结构同时用于 logs.other.wischoicer 与 task.private_data 异步快照。
type WischoicerAttribution struct {
	SchemaVersion    int    `json:"schema_version"`
	SourceService    string `json:"source_service"`
	InternalFunction bool   `json:"internal_function"`
	FeatureCode      string `json:"feature_code"`
	FeatureName      string `json:"feature_name,omitempty"`
	OperationCode    string `json:"operation_code"`
	OperationName    string `json:"operation_name,omitempty"`
	BizTaskId        string `json:"biz_task_id"`
	BizTaskTitle     string `json:"biz_task_title,omitempty"`
	SubTaskId        string `json:"sub_task_id,omitempty"`
	// AccountId 为归一化后的 effective_account_id = account_id || app_user_id
	// （回炉收敛 b9996b28：v1 MVP account_id == app_user_id，落库只写一个账号事实源）。
	AccountId string `json:"account_id"`
	AppUserId string `json:"app_user_id"`
}

// ParseWischoicerAttribution 从入站 gin 上下文解析 X-Wischoicer-* header。
// 仅当 source_service=content-workstation 且 internal_function=true 时返回非 nil；
// 否则该请求不进入功能计费归因（RFC §2.1：只有 true 才进入功能计费归因）。
//
// 任一 required header 缺失时不阻断主流程，只是对应字段为空（feature_code 缺失/未知
// 会在查询侧归入 uncategorized）。account_id 归一化为 effective_account_id。
func ParseWischoicerAttribution(c *gin.Context) *WischoicerAttribution {
	if c == nil || c.Request == nil {
		return nil
	}
	sourceService := c.GetHeader(WischoicerHeaderSourceService)
	if sourceService != WischoicerSourceServiceContentWorkstation {
		return nil
	}
	if !parseWischoicerBoolHeader(c.GetHeader(WischoicerHeaderInternalFunction)) {
		return nil
	}
	appUserId := c.GetHeader(WischoicerHeaderAppUserId)
	accountHeader := c.GetHeader(WischoicerHeaderAccountId)
	// effective_account_id = account_id || app_user_id
	effectiveAccountId := accountHeader
	if effectiveAccountId == "" {
		effectiveAccountId = appUserId
	}
	return &WischoicerAttribution{
		SchemaVersion:    wischoicerSchemaVersion,
		SourceService:    WischoicerSourceServiceContentWorkstation,
		InternalFunction: true,
		FeatureCode:      c.GetHeader(WischoicerHeaderFeatureCode),
		FeatureName:      c.GetHeader(WischoicerHeaderFeatureName),
		OperationCode:    c.GetHeader(WischoicerHeaderOperationCode),
		OperationName:    c.GetHeader(WischoicerHeaderOperationName),
		BizTaskId:        c.GetHeader(WischoicerHeaderBizTaskId),
		BizTaskTitle:     c.GetHeader(WischoicerHeaderBizTaskTitle),
		SubTaskId:        c.GetHeader(WischoicerHeaderSubTaskId),
		AccountId:        effectiveAccountId,
		AppUserId:        appUserId,
	}
}

// ToMap 构造 logs.other.wischoicer 的嵌套对象（含 billing_stage，由落库点传入）。
// 调用方将其合并进 Other map 后由 common.MapToJsonStr 序列化。
func (a *WischoicerAttribution) ToMap(billingStage string) map[string]interface{} {
	if a == nil {
		return nil
	}
	m := map[string]interface{}{
		"schema_version":    a.SchemaVersion,
		"source_service":    a.SourceService,
		"internal_function": a.InternalFunction,
		"feature_code":      a.FeatureCode,
		"operation_code":    a.OperationCode,
		"biz_task_id":       a.BizTaskId,
		"account_id":        a.AccountId,
		"app_user_id":       a.AppUserId,
		"billing_stage":     billingStage,
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
	if a.SubTaskId != "" {
		m["sub_task_id"] = a.SubTaskId
	}
	return m
}

// parseWischoicerBoolHeader 仅接受字符串 "true"（大小写不敏感）为真，其余为假。
func parseWischoicerBoolHeader(v string) bool {
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false
	}
	return b
}

// WischoicerAttributionFromMap 从 logs.other 解析出的 wischoicer 子对象反向构造结构。
// 查询侧聚合时用于把 JSON 行还原为强类型字段。缺字段不报错，按零值处理。
func WischoicerAttributionFromMap(m map[string]interface{}) *WischoicerAttribution {
	if m == nil {
		return nil
	}
	return &WischoicerAttribution{
		SchemaVersion:    intFromAny(m["schema_version"]),
		SourceService:    stringFromAny(m["source_service"]),
		InternalFunction: boolFromAny(m["internal_function"]),
		FeatureCode:      stringFromAny(m["feature_code"]),
		FeatureName:      stringFromAny(m["feature_name"]),
		OperationCode:    stringFromAny(m["operation_code"]),
		OperationName:    stringFromAny(m["operation_name"]),
		BizTaskId:        stringFromAny(m["biz_task_id"]),
		BizTaskTitle:     stringFromAny(m["biz_task_title"]),
		SubTaskId:        stringFromAny(m["sub_task_id"]),
		AccountId:        stringFromAny(m["account_id"]),
		AppUserId:        stringFromAny(m["app_user_id"]),
	}
}

// stringFromAny 把 JSON 反序列化出的任意值安全转成字符串。
func stringFromAny(v interface{}) string {
	if v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		return val
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(val)
	default:
		b, _ := common.Marshal(val)
		return string(b)
	}
}

// intFromAny 把 JSON 反序列化出的任意值安全转成 int（JSON 数字默认为 float64）。
func intFromAny(v interface{}) int {
	if v == nil {
		return 0
	}
	switch val := v.(type) {
	case float64:
		return int(val)
	case int:
		return val
	case string:
		n, _ := strconv.Atoi(val)
		return n
	}
	return 0
}

// boolFromAny 把 JSON 反序列化出的任意值安全转成 bool。
func boolFromAny(v interface{}) bool {
	if v == nil {
		return false
	}
	switch val := v.(type) {
	case bool:
		return val
	case string:
		return parseWischoicerBoolHeader(val)
	case float64:
		return val != 0
	}
	return false
}
