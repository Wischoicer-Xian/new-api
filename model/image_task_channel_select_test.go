package model

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests drive ListImageCapableChannelsForGroupModel against the DB path
// (MemoryCacheEnabled=false) and the cache path. They lock the model-layer
// image-capable gate: a channel is enumerated only when enabled AND configured
// for image tasks. The adapter-capability check lives in the service layer, so
// these tests do not assert it.

func truncateChannelAbility(t *testing.T) {
	t.Helper()
	require.NoError(t, DB.Where("1=1").Delete(&Channel{}).Error)
	require.NoError(t, DB.Where("1=1").Delete(&Ability{}).Error)
}

func seedImageChannel(t *testing.T, id int, group string, models []string, enabled, imageConfigured bool) *Channel {
	t.Helper()
	status := common.ChannelStatusEnabled
	if !enabled {
		status = common.ChannelStatusManuallyDisabled
	}
	ch := &Channel{
		Id:     id,
		Type:   constant.ChannelTypeOpenAI,
		Status: status,
		Group:  group,
		Models: strings.Join(models, ","),
		Key:    "sk-seed",
	}
	if imageConfigured {
		cfg := `{"defaults":{"generation":"sync"}}`
		ch.ImageExecutionConfig = &cfg
	}
	require.NoError(t, DB.Create(ch).Error)
	for _, m := range models {
		require.NoError(t, DB.Create(&Ability{Group: group, Model: m, ChannelId: id, Enabled: true}).Error)
	}
	return ch
}

func useDBChannelPath(t *testing.T) {
	t.Helper()
	prev := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = prev })
}

func TestListImageCapableChannelsForGroupModel_FiltersDisabledAndUnconfigured(t *testing.T) {
	useDBChannelPath(t)
	truncateChannelAbility(t)

	seedImageChannel(t, 101, "default", []string{"dall-e-3"}, true, true)
	seedImageChannel(t, 102, "default", []string{"dall-e-3"}, false, true) // disabled → filtered
	seedImageChannel(t, 103, "default", []string{"dall-e-3"}, true, false) // no image config → filtered

	got := ListImageCapableChannelsForGroupModel("default", "dall-e-3")
	require.Len(t, got, 1)
	assert.Equal(t, 101, got[0].Id)
}

func TestListImageCapableChannelsForGroupModel_EmptyWhenNoMatch(t *testing.T) {
	useDBChannelPath(t)
	truncateChannelAbility(t)

	seedImageChannel(t, 201, "default", []string{"dall-e-3"}, true, true)

	assert.Nil(t, ListImageCapableChannelsForGroupModel("vip", "dall-e-3"))   // wrong group
	assert.Nil(t, ListImageCapableChannelsForGroupModel("default", "gpt-4o")) // wrong model
	assert.Nil(t, ListImageCapableChannelsForGroupModel("", "dall-e-3"))      // empty group
	assert.Nil(t, ListImageCapableChannelsForGroupModel("default", ""))       // empty model
}

func TestListImageCapableChannelsForGroupModel_NormalizedModelFallback(t *testing.T) {
	useDBChannelPath(t)
	truncateChannelAbility(t)

	// Ability registered on the canonical wildcard gpt-4-gizmo-*; a request for
	// the specific gpt-4-gizmo-foo name resolves via the normalized fallback,
	// mirroring IsChannelEnabledForGroupModel.
	seedImageChannel(t, 301, "default", []string{"gpt-4-gizmo-*"}, true, true)

	got := ListImageCapableChannelsForGroupModel("default", "gpt-4-gizmo-foo")
	require.Len(t, got, 1)
	assert.Equal(t, 301, got[0].Id)

	// The exact wildcard name resolves too.
	got = ListImageCapableChannelsForGroupModel("default", "gpt-4-gizmo-*")
	require.Len(t, got, 1)
}

func TestListImageCapableChannelsForGroupModel_MemoryCachePath(t *testing.T) {
	prev := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	t.Cleanup(func() { common.MemoryCacheEnabled = prev })
	truncateChannelAbility(t)

	seedImageChannel(t, 401, "default", []string{"dall-e-3"}, true, true)
	// Rebuild the cache from DB so the channel enters group2model2channels; a
	// disabled/unconfigured sibling must stay out of the cache pool.
	seedImageChannel(t, 402, "default", []string{"dall-e-3"}, false, true)
	InitChannelCache()

	got := ListImageCapableChannelsForGroupModel("default", "dall-e-3")
	require.Len(t, got, 1)
	assert.Equal(t, 401, got[0].Id)
}
