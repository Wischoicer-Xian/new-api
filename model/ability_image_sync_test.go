package model

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WIS-580: synchronous image relay paths (/v1/images/generations, /v1/images/edits,
// /v1/edits) must NOT select channels whose image provider is async/task-only —
// e.g. ChannelTypeApiNebula, served via /v1/image-tasks/* + ChannelRevision. Such
// types have no entry in relay.GetAdaptor's switch, so if one is selected for a
// sync image request, relay.ImageHelper hits GetAdaptor(info.ApiType)==nil and
// returns "invalid api type" 500.
//
// model cannot import service (import cycle), so the eligibility truth is INJECTED
// via SyncImageChannelEligibility by service.init. These tests stub that hook to
// exercise the filter mechanism; the real (registry-backed) predicate is tested in
// service (TestChannelSupportsSyncImage / TestSyncImageEligibilityHookRegistered).

// withStubSyncImageEligibility installs a predicate that mimics the real
// service.ChannelSupportsSyncImage contract — exclude ChannelTypeApiNebula
// (async-only), allow everything else — and restores the prior value afterwards.
func withStubSyncImageEligibility(t *testing.T) {
	t.Helper()
	prev := SyncImageChannelEligibility
	SyncImageChannelEligibility = func(channelType int) bool {
		return channelType != constant.ChannelTypeApiNebula
	}
	t.Cleanup(func() { SyncImageChannelEligibility = prev })
}

func TestIsSyncImagePath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/v1/images/generations", true},
		{"/v1/images/generations?n=1", true},
		{"/v1/images/edits", true},
		{"/v1/edits", true},
		{"/v1/chat/completions", false},
		{"/v1/image-tasks/generations", false}, // async path, served via ChannelRevision
		{"", false},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, IsSyncImagePath(c.path), "IsSyncImagePath(%q)", c.path)
	}
}

// With no injected predicate, model must exclude NO type — the rule is permissive
// by default so it can never silently drop a channel (e.g. if service init has not
// run, or in a model-only test binary).
func TestChannelTypeSupportsSyncImage_PermissiveWhenHookNil(t *testing.T) {
	prev := SyncImageChannelEligibility
	SyncImageChannelEligibility = nil
	t.Cleanup(func() { SyncImageChannelEligibility = prev })
	assert.True(t, channelTypeSupportsSyncImage(constant.ChannelTypeApiNebula))
	assert.True(t, channelTypeSupportsSyncImage(constant.ChannelTypeOpenAI))
}

func TestChannelTypeSupportsSyncImage_DelegatesToHook(t *testing.T) {
	withStubSyncImageEligibility(t)
	assert.False(t, channelTypeSupportsSyncImage(constant.ChannelTypeApiNebula),
		"ApiNebula is async-only (no sync GetAdaptor case) -> not selectable for sync image")
	assert.True(t, channelTypeSupportsSyncImage(constant.ChannelTypeOpenAI), "OpenAI has a sync image adaptor")
	assert.True(t, channelTypeSupportsSyncImage(constant.ChannelTypeAdvancedCustom), "Advanced Custom is path-matched separately")
}

func TestExcludeChannelForSyncImage(t *testing.T) {
	withStubSyncImageEligibility(t)
	// THE BUG CASE: a sync image request must exclude the async-only channel.
	assert.True(t, excludeChannelForSyncImage("/v1/images/generations", constant.ChannelTypeApiNebula),
		"sync image must not route to async-only ApiNebula channel (WIS-580 root cause)")
	assert.True(t, excludeChannelForSyncImage("/v1/images/edits", constant.ChannelTypeApiNebula))
	// Sync-capable channels are kept for sync image paths.
	assert.False(t, excludeChannelForSyncImage("/v1/images/generations", constant.ChannelTypeOpenAI))
	assert.False(t, excludeChannelForSyncImage("/v1/images/generations", constant.ChannelTypeAdvancedCustom))
	// Non-sync-image paths do not trigger exclusion here.
	assert.False(t, excludeChannelForSyncImage("/v1/chat/completions", constant.ChannelTypeApiNebula))
	assert.False(t, excludeChannelForSyncImage("/v1/image-tasks/generations", constant.ChannelTypeApiNebula))
	assert.False(t, excludeChannelForSyncImage("", constant.ChannelTypeApiNebula))
}

// The exclusion decision must come from the injected predicate, NOT a hardcoded
// map: a sentinel type the stub rejects is excluded even though model has no
// static knowledge of it. This is the guard against duplicating capability truth.
func TestExcludeChannelForSyncImage_HookDriven(t *testing.T) {
	const sentinelAsyncOnly = 99999
	prev := SyncImageChannelEligibility
	SyncImageChannelEligibility = func(channelType int) bool {
		return channelType != sentinelAsyncOnly
	}
	t.Cleanup(func() { SyncImageChannelEligibility = prev })
	assert.True(t, excludeChannelForSyncImage("/v1/images/generations", sentinelAsyncOnly),
		"a type the injected predicate rejects must be excluded even though model has no static knowledge of it")
	assert.False(t, excludeChannelForSyncImage("/v1/chat/completions", sentinelAsyncOnly))
}

// TestFilterAbilitiesByRequestPathAndModel_ExcludesAsyncOnlyImageChannel is the
// end-to-end repro for WIS-580: a synchronous /v1/images/generations request for
// gpt-image-2, with the model homed on both a sync-capable (OpenAI type-1)
// channel and an async-only (ApiNebula type-59) channel, must select ONLY the
// sync-capable channel. Before the fix, the type-59 channel could be selected
// and relay.ImageHelper returned "invalid api type: 36" 500.
func TestFilterAbilitiesByRequestPathAndModel_ExcludesAsyncOnlyImageChannel(t *testing.T) {
	withStubSyncImageEligibility(t)
	chSync := Channel{Type: constant.ChannelTypeOpenAI, Status: 1, Group: "default", Models: "gpt-image-2", Name: "sync-image"}
	chAsync := Channel{Type: constant.ChannelTypeApiNebula, Status: 1, Group: "default", Models: "gpt-image-2", Name: "async-image"}
	require.NoError(t, DB.Create(&chSync).Error)
	require.NoError(t, DB.Create(&chAsync).Error)
	t.Cleanup(func() {
		DB.Delete(&Channel{}, chSync.Id)
		DB.Delete(&Channel{}, chAsync.Id)
	})

	abilities := []Ability{
		{Group: "default", Model: "gpt-image-2", ChannelId: chSync.Id, Enabled: true},
		{Group: "default", Model: "gpt-image-2", ChannelId: chAsync.Id, Enabled: true},
	}

	// Sync image path: async-only (type-59) channel must be excluded.
	got := filterAbilitiesByRequestPathAndModel(abilities, "/v1/images/generations", "gpt-image-2")
	assert.Len(t, got, 1, "sync image must drop the async-only channel")
	assert.Equal(t, chSync.Id, got[0].ChannelId, "only the sync-capable channel remains")

	// Non-image path: neither channel is excluded by the sync-image rule.
	gotChat := filterAbilitiesByRequestPathAndModel(abilities, "/v1/chat/completions", "gpt-image-2")
	assert.Len(t, gotChat, 2, "non-image paths do not exclude by sync-image capability")
}

// TestFilterChannelsByRequestPathAndModel_ExcludesAsyncOnlyImageChannel covers the
// MEMORY_CACHE_ENABLED selection path: candidates are channel IDs resolved from
// the in-memory channelsIDM cache. The async-only (type-59) channel must be
// dropped for sync image paths, same as the DB path (WIS-580 记星 cache on/off).
func TestFilterChannelsByRequestPathAndModel_ExcludesAsyncOnlyImageChannel(t *testing.T) {
	withStubSyncImageEligibility(t)

	syncID, asyncID := 9005801, 9005802
	chSync := &Channel{Id: syncID, Type: constant.ChannelTypeOpenAI}
	chAsync := &Channel{Id: asyncID, Type: constant.ChannelTypeApiNebula}

	// Seed the in-memory cache the filter reads (channelsIDM), under its lock.
	prev := channelsIDM
	channelSyncLock.Lock()
	channelsIDM = map[int]*Channel{syncID: chSync, asyncID: chAsync}
	t.Cleanup(func() {
		channelSyncLock.Lock()
		channelsIDM = prev
		channelSyncLock.Unlock()
	})
	defer channelSyncLock.Unlock()

	// Sync image path: async-only channel dropped, sync-capable channel kept.
	got := filterChannelsByRequestPathAndModel([]int{syncID, asyncID}, "/v1/images/generations", "gpt-image-2")
	assert.Equal(t, []int{syncID}, got, "cache path must drop async-only channel for sync image")

	// Non-image path: neither channel excluded by the sync-image rule.
	gotChat := filterChannelsByRequestPathAndModel([]int{syncID, asyncID}, "/v1/chat/completions", "gpt-4o")
	assert.ElementsMatch(t, []int{syncID, asyncID}, gotChat, "non-image path keeps both")
}
