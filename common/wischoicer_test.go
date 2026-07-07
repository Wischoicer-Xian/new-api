package common

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseWischoicerAttribution_HappyPath(t *testing.T) {
	h := http.Header{}
	h.Set(HeaderWischoicerSourceService, "content-workstation")
	h.Set(HeaderWischoicerInternalFunction, "true")
	h.Set(HeaderWischoicerFeatureCode, "merch_video_clone")
	h.Set(HeaderWischoicerFeatureName, "爆款复刻中心 - 带货视频爆款复刻")
	h.Set(HeaderWischoicerOperationCode, "merch_video_clone.step2.reference_sheet.generate")
	h.Set(HeaderWischoicerBizTaskID, "019f-task")
	h.Set(HeaderWischoicerAppUserID, "019f-user")
	h.Set(HeaderWischoicerAccountID, "019f-acct")

	a := ParseWischoicerAttribution(h)
	require.NotNil(t, a)
	assert.True(t, a.IsAttributed())
	assert.True(t, a.IsKnownFeature())
	assert.Equal(t, WischoicerSchemaVersion, a.SchemaVersion)
	// account_id 归一化：两者都传时优先 account_id。
	assert.Equal(t, "019f-acct", a.AccountID)
	assert.Equal(t, "019f-user", a.AppUserID)
}

func TestParseWischoicerAttribution_EffectiveAccountIDFallback(t *testing.T) {
	// 回炉 delta：account_id v1 optional，缺失时 effective_account_id = app_user_id。
	h := http.Header{}
	h.Set(HeaderWischoicerSourceService, "content-workstation")
	h.Set(HeaderWischoicerInternalFunction, "true")
	h.Set(HeaderWischoicerAppUserID, "019f-user")

	a := ParseWischoicerAttribution(h)
	require.NotNil(t, a)
	assert.Equal(t, "019f-user", a.AccountID, "effective_account_id should fall back to app_user_id")
}

func TestParseWischoicerAttribution_RejectsNonAttributed(t *testing.T) {
	cases := []struct {
		name string
		h    http.Header
	}{
		{"missing source service", func() http.Header {
			h := http.Header{}
			h.Set(HeaderWischoicerInternalFunction, "true")
			return h
		}()},
		{"wrong source service", func() http.Header {
			h := http.Header{}
			h.Set(HeaderWischoicerSourceService, "some-other-service")
			h.Set(HeaderWischoicerInternalFunction, "true")
			return h
		}()},
		{"internal_function false", func() http.Header {
			h := http.Header{}
			h.Set(HeaderWischoicerSourceService, "content-workstation")
			h.Set(HeaderWischoicerInternalFunction, "false")
			return h
		}()},
		{"internal_function missing", func() http.Header {
			h := http.Header{}
			h.Set(HeaderWischoicerSourceService, "content-workstation")
			return h
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 普通用户 API Key 调用绝不能误入归因统计。
			assert.Nil(t, ParseWischoicerAttribution(tc.h))
		})
	}
}

func TestParseWischoicerAttribution_InternalFunctionCaseInsensitive(t *testing.T) {
	h := http.Header{}
	h.Set(HeaderWischoicerSourceService, "content-workstation")
	h.Set(HeaderWischoicerInternalFunction, "TRUE")
	h.Set(HeaderWischoicerAppUserID, "u")
	assert.NotNil(t, ParseWischoicerAttribution(h))
}

func TestWischoicerIsKnownFeatureCode(t *testing.T) {
	assert.True(t, WischoicerIsKnownFeatureCode("reference_copy"))
	assert.True(t, WischoicerIsKnownFeatureCode("merch_video_clone"))
	assert.True(t, WischoicerIsKnownFeatureCode("image_creation"))
	assert.True(t, WischoicerIsKnownFeatureCode("copywriting_creation")) // 预留枚举仍算已知
	assert.True(t, WischoicerIsKnownFeatureCode("video_creation"))
	assert.False(t, WischoicerIsKnownFeatureCode(""))
	assert.False(t, WischoicerIsKnownFeatureCode("unknown_feature"))
}

func TestWischoicerAttribution_IsKnownFeature_UnknownGoesUncategorized(t *testing.T) {
	// feature_code 缺失/未知但归因有效 → uncategorized，不能误归到任何 feature。
	h := http.Header{}
	h.Set(HeaderWischoicerSourceService, "content-workstation")
	h.Set(HeaderWischoicerInternalFunction, "true")
	h.Set(HeaderWischoicerAppUserID, "u")
	// 故意不设 feature_code
	a := ParseWischoicerAttribution(h)
	require.NotNil(t, a)
	assert.True(t, a.IsAttributed())
	assert.False(t, a.IsKnownFeature(), "missing feature_code must be uncategorized")
}

func TestWischoicerAttribution_ToOtherMap_Contract(t *testing.T) {
	h := http.Header{}
	h.Set(HeaderWischoicerSourceService, "content-workstation")
	h.Set(HeaderWischoicerInternalFunction, "true")
	h.Set(HeaderWischoicerFeatureCode, "image_creation")
	h.Set(HeaderWischoicerOperationCode, "image_creation.edit")
	h.Set(HeaderWischoicerBizTaskID, "task-1")
	h.Set(HeaderWischoicerAppUserID, "u-1")
	a := ParseWischoicerAttribution(h)
	require.NotNil(t, a)

	m := a.ToOtherMap(WischoicerStageRequest)
	assert.Equal(t, "content-workstation", m["source_service"])
	assert.Equal(t, true, m["internal_function"])
	assert.Equal(t, "image_creation", m["feature_code"])
	assert.Equal(t, "image_creation.edit", m["operation_code"])
	assert.Equal(t, "task-1", m["biz_task_id"])
	assert.Equal(t, WischoicerSchemaVersion, m["schema_version"])
	assert.Equal(t, WischoicerStageRequest, m["billing_stage"])
	// account_id 归一化值落库。
	assert.Equal(t, "u-1", m["account_id"])

	// billing_stage 非法时不写入。
	m2 := a.ToOtherMap("bogus")
	_, has := m2["billing_stage"]
	assert.False(t, has)
}
