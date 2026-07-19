package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
)

// WIS-515 read-side masking tests. These cover the gap left by WIS-505's
// write-time masking: historical rows (or any row that stored a real model_name
// for a hidden system token) must never surface the real model on user-facing
// reads. Identification is always token_id + hidden, never a model-name string,
// so a normal API key calling the same real model is never affected.

const (
	wis515User = 515001
	wis515Ts   = int64(1750000000)
)

func wis515SeedToken(t *testing.T, id, userId int, hidden bool, key, name string) *Token {
	t.Helper()
	tok := &Token{Id: id, UserId: userId, Key: key, Name: name, Hidden: hidden, Status: 1, ExpiredTime: -1}
	require.NoError(t, DB.Create(tok).Error)
	t.Cleanup(func() { DB.Unscoped().Delete(tok) })
	return tok
}

func wis515SeedLog(t *testing.T, userId, tokenId int, modelName, requestID string) {
	t.Helper()
	l := &Log{UserId: userId, TokenId: tokenId, ModelName: modelName, RequestId: requestID, Type: LogTypeConsume, CreatedAt: wis515Ts}
	require.NoError(t, LOG_DB.Create(l).Error)
	t.Cleanup(func() { LOG_DB.Unscoped().Where("request_id = ?", requestID).Delete(&Log{}) })
}

func wis515SeedQuotaData(t *testing.T, userId, tokenId int, useGroup, modelName string, quota int) {
	t.Helper()
	row := &QuotaData{
		UserID:    userId,
		Username:  "wis515-user",
		ModelName: modelName,
		CreatedAt: wis515Ts,
		UseGroup:  useGroup,
		TokenID:   tokenId,
		Count:     1,
		Quota:     quota,
	}
	require.NoError(t, DB.Create(row).Error)
	t.Cleanup(func() {
		DB.Unscoped().Where("user_id = ? AND token_id = ? AND model_name = ?", userId, tokenId, modelName).Delete(&QuotaData{})
	})
}

func modelNamesByRequest(logs []*Log) map[string]string {
	out := make(map[string]string, len(logs))
	for _, l := range logs {
		out[l.RequestId] = l.ModelName
	}
	return out
}

// TestGetUserLogsMasksHiddenSystemToken: the hidden system token row is masked
// to the alias on the user read path, while a normal API key calling the SAME
// real model keeps the real name — proving there is no model-name mapping.
func TestGetUserLogsMasksHiddenSystemToken(t *testing.T) {
	const real = "claude-opus-4-wis515"
	hidden := wis515SeedToken(t, 515101, wis515User, true, "sk-wis515-hidden", "hidden-sys")
	normal := wis515SeedToken(t, 515102, wis515User, false, "sk-wis515-normal", "normal")
	wis515SeedLog(t, wis515User, hidden.Id, real, "wis515-log-hidden")
	wis515SeedLog(t, wis515User, normal.Id, real, "wis515-log-normal")

	logs, _, err := GetUserLogs(wis515User, LogTypeUnknown, 0, 0, "", "", 0, 100, "", "", "")
	require.NoError(t, err)
	byReq := modelNamesByRequest(logs)
	require.Equal(t, common.MaskedSystemModelAlias, byReq["wis515-log-hidden"], "hidden system token must be masked")
	require.Equal(t, real, byReq["wis515-log-normal"], "normal token must keep the real model even on the same model")
}

// TestGetUserLogsAliasFilterSurfacesSystemRows: filtering by the alias returns
// the hidden system row (whose historical model_name is the real model), and
// TestGetUserLogsRealModelFilterHidesSystemRows: filtering by the real model
// never surfaces the hidden system row.
func TestGetUserLogsAliasFilterSurfacesSystemRows(t *testing.T) {
	const real = "gemini-2.5-pro-wis515"
	hidden := wis515SeedToken(t, 515111, wis515User, true, "sk-wis515-hidden-f2", "hidden-sys")
	wis515SeedLog(t, wis515User, hidden.Id, real, "wis515-f2-hidden")

	logs, _, err := GetUserLogs(wis515User, LogTypeUnknown, 0, 0, common.MaskedSystemModelAlias, "", 0, 100, "", "", "")
	require.NoError(t, err)
	require.Len(t, logs, 1)
	require.Equal(t, common.MaskedSystemModelAlias, logs[0].ModelName)
	require.Equal(t, "wis515-f2-hidden", logs[0].RequestId)
}

func TestGetUserLogsRealModelFilterHidesSystemRows(t *testing.T) {
	const real = "gemini-2.5-pro-wis515-f3"
	hidden := wis515SeedToken(t, 515121, wis515User, true, "sk-wis515-hidden-f3", "hidden-sys")
	normal := wis515SeedToken(t, 515122, wis515User, false, "sk-wis515-normal-f3", "normal")
	wis515SeedLog(t, wis515User, hidden.Id, real, "wis515-f3-hidden")
	wis515SeedLog(t, wis515User, normal.Id, real, "wis515-f3-normal")

	logs, _, err := GetUserLogs(wis515User, LogTypeUnknown, 0, 0, real, "", 0, 100, "", "", "")
	require.NoError(t, err)
	byReq := modelNamesByRequest(logs)
	_, hasHidden := byReq["wis515-f3-hidden"]
	require.False(t, hasHidden, "real-model filter must not surface hidden system rows")
	require.Equal(t, real, byReq["wis515-f3-normal"], "normal token must still be visible under its real model")
}

// TestGetLogByTokenIdMasksHiddenToken: the by-key self log view masks every row
// when the token itself is hidden.
func TestGetLogByTokenIdMasksHiddenToken(t *testing.T) {
	const real = "kling-v2-wis515"
	hidden := wis515SeedToken(t, 515131, wis515User, true, "sk-wis515-hidden-bykey", "hidden-sys")
	normal := wis515SeedToken(t, 515132, wis515User, false, "sk-wis515-normal-bykey", "normal")
	wis515SeedLog(t, wis515User, hidden.Id, real, "wis515-bykey-hidden")
	wis515SeedLog(t, wis515User, normal.Id, real, "wis515-bykey-normal")

	hiddenLogs, err := GetLogByTokenId(hidden.Id)
	require.NoError(t, err)
	for _, l := range hiddenLogs {
		require.Equal(t, common.MaskedSystemModelAlias, l.ModelName)
	}

	normalLogs, err := GetLogByTokenId(normal.Id)
	require.NoError(t, err)
	for _, l := range normalLogs {
		require.Equal(t, real, l.ModelName)
	}
}

// TestGetQuotaDataByUserIdCollapsesHiddenSystemBucket: the user-facing
// consumption distribution collapses one hidden system token's multiple real
// models into a single alias bucket (quota summed), while a normal API key
// calling the same real model keeps its own real-model bucket — never merged.
func TestGetQuotaDataByUserIdCollapsesHiddenSystemBucket(t *testing.T) {
	const (
		modelA = "doubao-seedance-wis515"
		modelB = "gpt-image-wis515"
	)
	hidden := wis515SeedToken(t, 515141, wis515User, true, "sk-wis515-hidden-qd", "hidden-sys")
	normal := wis515SeedToken(t, 515142, wis515User, false, "sk-wis515-normal-qd", "normal")
	wis515SeedQuotaData(t, wis515User, hidden.Id, "default", modelA, 10)
	wis515SeedQuotaData(t, wis515User, hidden.Id, "default", modelB, 20)
	wis515SeedQuotaData(t, wis515User, normal.Id, "default", modelA, 5)

	rows, err := GetQuotaDataByUserId(wis515User, wis515Ts, wis515Ts+3600)
	require.NoError(t, err)

	byModel := map[string]int{}
	for _, r := range rows {
		byModel[r.ModelName] += r.Quota
	}
	require.Equal(t, 30, byModel[common.MaskedSystemModelAlias], "hidden token's two models must collapse into one alias bucket summing 30")
	require.Equal(t, 5, byModel[modelA], "normal token's real model A must keep its own bucket")
	require.Equal(t, 0, byModel[modelB], "the hidden token's real model B must not appear as its own bucket")
}

// TestGetSelfFlowQuotaDataMasksHiddenToken: the user-facing flow view masks the
// hidden system token's real models (same quota_data surface) and keeps the
// normal token's real model.
func TestGetSelfFlowQuotaDataMasksHiddenToken(t *testing.T) {
	const (
		modelA = "veo-wis515"
		modelB = "sora-wis515"
	)
	hidden := wis515SeedToken(t, 515151, wis515User, true, "sk-wis515-hidden-flow", "hidden-sys")
	normal := wis515SeedToken(t, 515152, wis515User, false, "sk-wis515-normal-flow", "normal")
	wis515SeedQuotaData(t, wis515User, hidden.Id, "default", modelA, 7)
	wis515SeedQuotaData(t, wis515User, hidden.Id, "default", modelB, 3)
	wis515SeedQuotaData(t, wis515User, normal.Id, "default", modelA, 9)

	rows, err := GetFlowQuotaData(wis515Ts, wis515Ts+3600, "", wis515User, common.RoleCommonUser)
	require.NoError(t, err)

	byToken := map[int]string{}
	for _, r := range rows {
		byToken[r.TokenID] = r.ModelName
	}
	require.Equal(t, common.MaskedSystemModelAlias, byToken[hidden.Id], "hidden token flow rows must be masked to the alias")
	require.Equal(t, modelA, byToken[normal.Id], "normal token must keep its real model in the flow view")
}

// TestGetFeatureUsageDetailsMasksHiddenSystemToken: the 费用明细 details API
// (boundary #2) must not return a raw model_name for the user's hidden system
// token, while a normal attributed token on the same model keeps the real name.
func TestGetFeatureUsageDetailsMasksHiddenSystemToken(t *testing.T) {
	const (
		fuUser = 515005
		real   = "doubao-seedance-wis515-fu"
	)
	hidden := wis515SeedToken(t, 515161, fuUser, true, "sk-wis515-hidden-fu", "hidden-sys")
	normal := wis515SeedToken(t, 515162, fuUser, false, "sk-wis515-normal-fu", "normal")
	seedAttributed := func(tokenId int, requestID, bizTask string) {
		require.NoError(t, LOG_DB.Create(&Log{
			UserId: fuUser, CreatedAt: wis515Ts, Type: LogTypeConsume, ModelName: real, TokenId: tokenId, Quota: 100,
			RequestId: requestID,
			Other:     buildOtherJSON("image_creation", "image_creation.generate", bizTask, "", common.WischoicerStageRequest, "", "", ""),
		}).Error)
		t.Cleanup(func() { LOG_DB.Unscoped().Where("request_id = ?", requestID).Delete(&Log{}) })
	}
	seedAttributed(hidden.Id, "wis515-fu-hidden", "biz-fu-h")
	seedAttributed(normal.Id, "wis515-fu-normal", "biz-fu-n")

	res, err := GetFeatureUsageDetails(fuUser, wis515Ts, wis515Ts+3600, FeatureUsageDetailsFilter{}, 1, 100)
	require.NoError(t, err)
	modelByToken := map[int]string{}
	for _, it := range res.Items {
		modelByToken[it.TokenID] = it.ModelName
	}
	require.Equal(t, common.MaskedSystemModelAlias, modelByToken[hidden.Id], "费用明细 must mask hidden system token model")
	require.Equal(t, real, modelByToken[normal.Id], "费用明细 must keep real model for normal attributed token")
}

// TestGetUserLogsMasksAfterHiddenTokenSoftDeleted: a rotated/soft-deleted system
// key's historical logs must STAY masked. The hidden set covers soft-deleted
// tokens (Unscoped read), so pre-WIS-505 rows never resurface the raw model
// after a system key rotation. (Regression for 记星 R2 P1.)
func TestGetUserLogsMasksAfterHiddenTokenSoftDeleted(t *testing.T) {
	const real = "claude-opus-4-wis515-sd"
	hidden := wis515SeedToken(t, 515171, wis515User, true, "sk-wis515-hidden-sd", "hidden-sys")
	wis515SeedLog(t, wis515User, hidden.Id, real, "wis515-sd-hidden")
	// Rotate / soft-delete the system key (mirrors Token.Delete in production).
	require.NoError(t, hidden.Delete())

	logs, _, err := GetUserLogs(wis515User, LogTypeUnknown, 0, 0, "", "", 0, 100, "", "", "")
	require.NoError(t, err)
	byReq := modelNamesByRequest(logs)
	require.Equal(t, common.MaskedSystemModelAlias, byReq["wis515-sd-hidden"],
		"historical log of a soft-deleted hidden system token must still be masked")
}

// TestGetFeatureUsageDetailsMasksAfterHiddenTokenSoftDeleted: same guarantee for
// the 费用明细 details API after the hidden system token is soft-deleted.
func TestGetFeatureUsageDetailsMasksAfterHiddenTokenSoftDeleted(t *testing.T) {
	const (
		fuUserSD = 515006
		realSD   = "doubao-seedance-wis515-sd-fu"
	)
	hidden := wis515SeedToken(t, 515181, fuUserSD, true, "sk-wis515-hidden-sd-fu", "hidden-sys")
	require.NoError(t, LOG_DB.Create(&Log{
		UserId: fuUserSD, CreatedAt: wis515Ts, Type: LogTypeConsume, ModelName: realSD, TokenId: hidden.Id, Quota: 100,
		RequestId: "wis515-sd-fu-hidden",
		Other:     buildOtherJSON("image_creation", "image_creation.generate", "biz-fu-sd", "", common.WischoicerStageRequest, "", "", ""),
	}).Error)
	t.Cleanup(func() { LOG_DB.Unscoped().Where("request_id = ?", "wis515-sd-fu-hidden").Delete(&Log{}) })
	// Rotate / soft-delete the system key after the historical row exists.
	require.NoError(t, hidden.Delete())

	res, err := GetFeatureUsageDetails(fuUserSD, wis515Ts, wis515Ts+3600, FeatureUsageDetailsFilter{}, 1, 100)
	require.NoError(t, err)
	modelByToken := map[int]string{}
	for _, it := range res.Items {
		modelByToken[it.TokenID] = it.ModelName
	}
	require.Equal(t, common.MaskedSystemModelAlias, modelByToken[hidden.Id],
		"费用明细 must keep masking after the hidden system token is soft-deleted")
}
