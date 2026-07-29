package service

import (
	"fmt"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WIS-580 P2 async regression (记星 review round 2): the sync-image candidate
// filtering must NOT touch the async /v1/image-tasks/* path. This DRIVES the real
// async candidate enumeration (model.ListImageCapableChannelsForGroupModel) and
// selection (trySelectImageTaskChannel) with an ApiNebula channel + its
// frozen revision, asserting type-59 is still selectable end-to-end. If the
// sync-image fix leaked into the async pool, this fails.
func TestAsyncImageTaskSelection_StillSelectsApiNebula(t *testing.T) {
	setupCreateTest(t)

	const chID = 7059
	cfg := `{"defaults":{"generation":"async_task","edit":"async_task"}}`
	require.NoError(t, model.DB.Create(&model.Channel{
		Id:                   chID,
		Type:                 constant.ChannelTypeApiNebula,
		Status:               common.ChannelStatusEnabled,
		Group:                "default",
		Models:               "gpt-image-2",
		Key:                  "sk-apinebula",
		ImageExecutionConfig: &cfg,
	}).Error)
	require.NoError(t, model.DB.Create(&model.Ability{
		Group: "default", Model: "gpt-image-2", ChannelId: chID, Enabled: true,
	}).Error)
	require.NoError(t, model.DB.Create(&model.ChannelRevision{
		ChannelID:      chID,
		RevisionNumber: 1,
		Endpoint:       "https://apinebula.example",
		CredentialRef:  fmt.Sprintf("channel:%d", chID),
		AdapterVersion: constant.ImageAdapterVersionApiNebula,
		Settings:       mustImageRevisionSettings(t, cfg),
	}).Error)

	// 1) Async candidate enumeration must still surface the type-59 channel.
	candidates := model.ListImageCapableChannelsForGroupModel("default", "gpt-image-2")
	require.Len(t, candidates, 1, "async pool must still contain the ApiNebula channel")
	assert.Equal(t, chID, candidates[0].Id)
	assert.Equal(t, constant.ChannelTypeApiNebula, candidates[0].Type)

	// 2) Async selection must still resolve it via capability + revision.
	sel, ok := trySelectImageTaskChannel(candidates[0], ImageOperationGeneration, "gpt-image-2")
	require.True(t, ok, "type-59 + revision must still be selectable for async generation")
	assert.Equal(t, ImageExecutionAsyncTask, sel.Mode, "ApiNebula resolves to async_task")
	assert.Equal(t, constant.ImageAdapterVersionApiNebula, sel.AdapterVersion)
}

// Companion: a sync-capable (OpenAI type-1) channel with a sync revision is
// selectable for async too (it is in the same pool), confirming the async path
// treats both channel types by capability, not by the sync-image rule.
func TestAsyncImageTaskSelection_AlsoSelectsSyncCapableOpenAI(t *testing.T) {
	setupCreateTest(t)

	const chID = 7001
	cfg := `{"defaults":{"generation":"sync","edit":"sync"}}`
	require.NoError(t, model.DB.Create(&model.Channel{
		Id:                   chID,
		Type:                 constant.ChannelTypeOpenAI,
		Status:               common.ChannelStatusEnabled,
		Group:                "default",
		Models:               "gpt-image-2",
		Key:                  "sk-openai",
		ImageExecutionConfig: &cfg,
	}).Error)
	require.NoError(t, model.DB.Create(&model.Ability{
		Group: "default", Model: "gpt-image-2", ChannelId: chID, Enabled: true,
	}).Error)
	require.NoError(t, model.DB.Create(&model.ChannelRevision{
		ChannelID:      chID,
		RevisionNumber: 1,
		Endpoint:       "https://api.openai.com",
		CredentialRef:  fmt.Sprintf("channel:%d", chID),
		AdapterVersion: "openai-image-adapter/v1",
		Settings:       mustImageRevisionSettings(t, cfg),
	}).Error)

	candidates := model.ListImageCapableChannelsForGroupModel("default", "gpt-image-2")
	require.Len(t, candidates, 1)
	sel, ok := trySelectImageTaskChannel(candidates[0], ImageOperationGeneration, "gpt-image-2")
	require.True(t, ok)
	assert.Equal(t, ImageExecutionSync, sel.Mode)
}
