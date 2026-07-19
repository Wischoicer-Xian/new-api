package model

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WIS-580: synchronous image relay paths (/v1/images/generations, /v1/images/edits,
// /v1/edits) must NOT select channels whose image provider is async/task-only —
// e.g. ChannelTypeApiNebula. model cannot import service (import cycle), so the
// eligibility truth is INJECTED via SyncImageChannelEligibility by service.init.
// These tests stub that hook to exercise the filter mechanism; the real
// (registry-backed, operation-granular) predicate is tested in service.

// withStubSyncImageEligibility installs a predicate that mimics the real
// service.ChannelSupportsSyncImageForOp contract — ChannelTypeApiNebula is
// async-only for every operation, everything else is allowed — and restores the
// prior value afterwards.
func withStubSyncImageEligibility(t *testing.T) {
	t.Helper()
	prev := SyncImageChannelEligibility
	SyncImageChannelEligibility = func(channelType int, _ SyncImageOperation) bool {
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

func TestSyncImageOperationFromPath(t *testing.T) {
	assert.Equal(t, SyncImageOpGeneration, SyncImageOperationFromPath("/v1/images/generations"))
	assert.Equal(t, SyncImageOpGeneration, SyncImageOperationFromPath("/v1/images/generations?n=1"))
	assert.Equal(t, SyncImageOpEdit, SyncImageOperationFromPath("/v1/images/edits"))
	assert.Equal(t, SyncImageOpEdit, SyncImageOperationFromPath("/v1/edits"))
	assert.Equal(t, SyncImageOpUnknown, SyncImageOperationFromPath("/v1/chat/completions"))
	assert.Equal(t, SyncImageOpUnknown, SyncImageOperationFromPath("/v1/image-tasks/generations"))
	assert.Equal(t, SyncImageOpUnknown, SyncImageOperationFromPath(""))
}

// With no injected predicate, model must exclude NO type — the rule is permissive
// by default so it can never silently drop a channel.
func TestExcludeChannelForSyncImage_PermissiveWhenHookNil(t *testing.T) {
	prev := SyncImageChannelEligibility
	SyncImageChannelEligibility = nil
	t.Cleanup(func() { SyncImageChannelEligibility = prev })
	assert.False(t, excludeChannelForSyncImage("/v1/images/generations", constant.ChannelTypeApiNebula))
	assert.False(t, excludeChannelForSyncImage("/v1/images/edits", constant.ChannelTypeApiNebula))
}

func TestExcludeChannelForSyncImage(t *testing.T) {
	withStubSyncImageEligibility(t)
	// THE BUG CASE: a sync image request must exclude the async-only channel,
	// for BOTH generation and edit paths (operation granularity, WIS-580 P2).
	assert.True(t, excludeChannelForSyncImage("/v1/images/generations", constant.ChannelTypeApiNebula),
		"sync generation must not route to async-only ApiNebula (WIS-580 root cause)")
	assert.True(t, excludeChannelForSyncImage("/v1/images/edits", constant.ChannelTypeApiNebula),
		"sync edit must not route to async-only ApiNebula either")
	// Sync-capable channels are kept for sync image paths.
	assert.False(t, excludeChannelForSyncImage("/v1/images/generations", constant.ChannelTypeOpenAI))
	assert.False(t, excludeChannelForSyncImage("/v1/images/edits", constant.ChannelTypeOpenAI))
	assert.False(t, excludeChannelForSyncImage("/v1/images/generations", constant.ChannelTypeAdvancedCustom))
	// Non-sync-image paths do not trigger exclusion.
	assert.False(t, excludeChannelForSyncImage("/v1/chat/completions", constant.ChannelTypeApiNebula))
	assert.False(t, excludeChannelForSyncImage("/v1/image-tasks/generations", constant.ChannelTypeApiNebula))
	assert.False(t, excludeChannelForSyncImage("", constant.ChannelTypeApiNebula))
}

// The exclusion decision must come from the injected predicate, NOT a hardcoded
// map: a sentinel type the stub rejects is excluded even though model has no
// static knowledge of it.
func TestExcludeChannelForSyncImage_HookDriven(t *testing.T) {
	const sentinelAsyncOnly = 99999
	prev := SyncImageChannelEligibility
	SyncImageChannelEligibility = func(channelType int, _ SyncImageOperation) bool {
		return channelType != sentinelAsyncOnly
	}
	t.Cleanup(func() { SyncImageChannelEligibility = prev })
	assert.True(t, excludeChannelForSyncImage("/v1/images/generations", sentinelAsyncOnly),
		"a type the injected predicate rejects must be excluded even though model has no static knowledge of it")
	assert.False(t, excludeChannelForSyncImage("/v1/chat/completions", sentinelAsyncOnly))
}

// TestFilterAbilitiesByRequestPathAndModel_ExcludesAsyncOnlyImageChannel is the
// end-to-end repro for WIS-580: a sync /v1/images/generations request for
// gpt-image-2, homed on both a sync-capable (OpenAI type-1) and an async-only
// (ApiNebula type-59) channel, must select ONLY the sync-capable channel.
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
// MEMORY_CACHE_ENABLED selection path (WIS-580 记星 cache on/off).
func TestFilterChannelsByRequestPathAndModel_ExcludesAsyncOnlyImageChannel(t *testing.T) {
	withStubSyncImageEligibility(t)

	syncID, asyncID := 9005801, 9005802
	chSync := &Channel{Id: syncID, Type: constant.ChannelTypeOpenAI}
	chAsync := &Channel{Id: asyncID, Type: constant.ChannelTypeApiNebula}

	prev := channelsIDM
	channelSyncLock.Lock()
	channelsIDM = map[int]*Channel{syncID: chSync, asyncID: chAsync}
	t.Cleanup(func() {
		channelSyncLock.Lock()
		channelsIDM = prev
		channelSyncLock.Unlock()
	})
	defer channelSyncLock.Unlock()

	got := filterChannelsByRequestPathAndModel([]int{syncID, asyncID}, "/v1/images/generations", "gpt-image-2")
	assert.Equal(t, []int{syncID}, got, "cache path must drop async-only channel for sync image")

	gotChat := filterChannelsByRequestPathAndModel([]int{syncID, asyncID}, "/v1/chat/completions", "gpt-4o")
	assert.ElementsMatch(t, []int{syncID, asyncID}, gotChat, "non-image path keeps both")
}
