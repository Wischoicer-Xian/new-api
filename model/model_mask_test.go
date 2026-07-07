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

func TestMaskModelNameIfHidden(t *testing.T) {
	cases := []struct {
		name   string
		model  string
		hidden bool
		want   string
	}{
		{"hidden masks real model", "claude-opus-4", true, MaskedSystemModelName},
		{"non-hidden keeps real model", "claude-opus-4", false, "claude-opus-4"},
		{"hidden empty name stays empty", "", true, ""},
		{"non-hidden empty name stays empty", "", false, ""},
		{"already-alias hidden is idempotent", MaskedSystemModelName, true, MaskedSystemModelName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, MaskModelNameIfHidden(tc.model, tc.hidden))
		})
	}
}

// TestMaskedModelNameResolvesTokenHiddenFlag covers the token-id based lookup used
// by the log writers (RecordConsumeLog/RecordErrorLog/RecordTaskBillingLog), InitTask,
// and the 8 task adaptors: a Hidden token's model is masked, a normal token's model is
// preserved, and missing/invalid token ids fall through to the real name.
func TestMaskedModelNameResolvesTokenHiddenFlag(t *testing.T) {
	truncateTables(t)
	require.NoError(t, DB.Create(&Token{Id: 1001, UserId: 1, Key: "sk-hidden", Name: "知言云策系统账号", Hidden: true}).Error)
	require.NoError(t, DB.Create(&Token{Id: 1002, UserId: 1, Key: "sk-normal", Name: "normal", Hidden: false}).Error)

	require.Equal(t, MaskedSystemModelName, MaskedModelName(1001, "claude-opus-4"))
	require.Equal(t, "claude-opus-4", MaskedModelName(1002, "claude-opus-4"))
	require.Equal(t, "claude-opus-4", MaskedModelName(0, "claude-opus-4"), "no token id keeps real name")
	require.Equal(t, "claude-opus-4", MaskedModelName(9999, "claude-opus-4"), "missing token keeps real name")
	require.Equal(t, "", MaskedModelName(1001, ""), "empty name stays empty even for hidden token")
}

// TestWithRealModelNameAdminInfoPreservesRealNameForAdmin locks the R3 decision 2
// contract: when the model was masked, the real name is preserved under the
// admin-only Other.admin_info key (stripped by formatUserLogs for users, kept for
// admins); when nothing was masked the map is returned untouched.
func TestWithRealModelNameAdminInfoPreservesRealNameForAdmin(t *testing.T) {
	t.Run("masked writes admin_info.real_model_name", func(t *testing.T) {
		got := withRealModelNameAdminInfo(nil, "claude-opus-4", MaskedSystemModelName)
		adminInfo, ok := got["admin_info"].(map[string]interface{})
		require.True(t, ok, "admin_info map should be set")
		require.Equal(t, "claude-opus-4", adminInfo["real_model_name"])
	})

	t.Run("not masked returns input unchanged with no admin_info", func(t *testing.T) {
		in := map[string]interface{}{"a": 1}
		got := withRealModelNameAdminInfo(in, "gpt-4", "gpt-4")
		require.Len(t, got, 1)
		require.Equal(t, 1, got["a"])
		_, hasAdmin := got["admin_info"]
		require.False(t, hasAdmin)
	})

	t.Run("empty real name returns input unchanged", func(t *testing.T) {
		got := withRealModelNameAdminInfo(map[string]interface{}{"a": 1}, "", MaskedSystemModelName)
		require.Len(t, got, 1)
	})

	t.Run("preserves existing admin_info and other keys", func(t *testing.T) {
		in := map[string]interface{}{
			"other":      1,
			"admin_info": map[string]interface{}{"existing": "x"},
		}
		got := withRealModelNameAdminInfo(in, "claude-opus-4", MaskedSystemModelName)
		require.Equal(t, 1, got["other"])
		adminInfo := got["admin_info"].(map[string]interface{})
		require.Equal(t, "x", adminInfo["existing"], "pre-existing admin_info keys preserved")
		require.Equal(t, "claude-opus-4", adminInfo["real_model_name"])
	})

	t.Run("does not mutate input map", func(t *testing.T) {
		in := map[string]interface{}{"a": 1}
		_ = withRealModelNameAdminInfo(in, "claude-opus-4", MaskedSystemModelName)
		_, hasAdmin := in["admin_info"]
		require.False(t, hasAdmin, "caller map must not be mutated")
	})
}

// TestRecordConsumeLogMasksHiddenTokenAndPreservesAdminRealName is the end-to-end
// contract for desensitization point 1 (RecordConsumeLog writes logs + quota_data):
// a Hidden system token's model name is stored as the alias in both logs and
// quota_data, with the real name preserved under admin_info; a normal token keeps
// the real model name everywhere and gets no admin_info (no false masking).
func TestRecordConsumeLogMasksHiddenTokenAndPreservesAdminRealName(t *testing.T) {
	truncateTables(t)
	require.NoError(t, DB.Create(&User{Id: 1, Username: "alice"}).Error)
	require.NoError(t, DB.Create(&Token{Id: 2001, UserId: 1, Key: "sk-hidden", Name: "知言云策系统账号", Hidden: true}).Error)
	require.NoError(t, DB.Create(&Token{Id: 2002, UserId: 1, Key: "sk-normal", Name: "normal", Hidden: false}).Error)

	// quota_data is written through an in-memory cache; reset it so we can assert
	// exactly what RecordConsumeLog produced.
	common.DataExportEnabled = true
	t.Cleanup(func() { common.DataExportEnabled = false })
	CacheQuotaDataLock.Lock()
	CacheQuotaData = make(map[string]*QuotaData)
	CacheQuotaDataLock.Unlock()

	newCtx := func() *gin.Context {
		gin.SetMode(gin.TestMode)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
		c.Set("username", "alice")
		return c
	}

	record := func(tokenId int) {
		RecordConsumeLog(newCtx(), 1, RecordConsumeLogParams{
			ChannelId:        1,
			PromptTokens:     10,
			CompletionTokens: 20,
			ModelName:        "claude-opus-4",
			TokenName:        "tk",
			Quota:            5,
			Content:          "use",
			TokenId:          tokenId,
			UseTimeSeconds:   1,
			IsStream:         false,
			Group:            "default",
		})
	}

	t.Run("hidden token masked in logs and quota_data, real name kept for admin", func(t *testing.T) {
		record(2001)

		var logRow Log
		require.NoError(t, DB.Where("token_id = ?", 2001).First(&logRow).Error)
		require.Equal(t, MaskedSystemModelName, logRow.ModelName, "user-visible log model must be the alias")

		other, _ := common.StrToMap(logRow.Other)
		require.NotNil(t, other)
		adminInfo, ok := other["admin_info"].(map[string]interface{})
		require.True(t, ok, "admin_info must preserve the real model name")
		require.Equal(t, "claude-opus-4", adminInfo["real_model_name"])

		SaveQuotaDataCache()
		var qd QuotaData
		require.NoError(t, DB.Where("token_id = ?", 2001).First(&qd).Error)
		require.Equal(t, MaskedSystemModelName, qd.ModelName, "quota_data (dashboard) model must be the alias")
	})

	t.Run("normal token keeps real model name, no admin_info, no false masking", func(t *testing.T) {
		record(2002)

		var logRow Log
		require.NoError(t, DB.Where("token_id = ?", 2002).First(&logRow).Error)
		require.Equal(t, "claude-opus-4", logRow.ModelName, "non-hidden token must keep the real model name")

		if logRow.Other != "" {
			other, _ := common.StrToMap(logRow.Other)
			if other != nil {
				_, hasAdmin := other["admin_info"]
				require.False(t, hasAdmin, "non-hidden token must not get an admin_info real-model field")
			}
		}

		SaveQuotaDataCache()
		var qd QuotaData
		require.NoError(t, DB.Where("token_id = ?", 2002).First(&qd).Error)
		assert.Equal(t, "claude-opus-4", qd.ModelName, "non-hidden token quota_data keeps real model name")
	})
}
