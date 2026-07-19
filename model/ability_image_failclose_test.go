package model

import (
	"errors"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
)

// WIS-580 P1 (记星 review round 2): the DB selection path must FAIL-CLOSE for
// sync image requests when channel metadata is unavailable — either because the
// metadata query errored, or because a channel row is missing. Returning the
// unfiltered candidates (the old behavior) re-opens the sync->type-59
// "invalid api type: 36" 500 the fix is meant to close. These exercise the pure
// filter-by-metadata helper directly so no real DB fault is needed.

func TestFilterAbilitiesByChannelMetadata_QueryErrorFailsClosedForSyncImage(t *testing.T) {
	withStubSyncImageEligibility(t)
	abilities := []Ability{{ChannelId: 1}, {ChannelId: 2}}
	// Query error + sync image path -> fail-close (no candidates), even though
	// unfiltered abilities exist. Returning them could include an async-only
	// channel we can no longer identify.
	got := filterAbilitiesByChannelMetadata(abilities, "/v1/images/generations", "gpt-image-2", nil, errors.New("db down"))
	assert.Empty(t, got, "sync image must fail-close when channel metadata query fails (WIS-580 P1)")
}

func TestFilterAbilitiesByChannelMetadata_QueryErrorKeepsFailOpenForNonImage(t *testing.T) {
	withStubSyncImageEligibility(t)
	abilities := []Ability{{ChannelId: 1}, {ChannelId: 2}}
	// Non-image path preserves the historical fail-open: a metadata hiccup must
	// not block chat/embedding selection (their filtering is best-effort).
	got := filterAbilitiesByChannelMetadata(abilities, "/v1/chat/completions", "gpt-4o", nil, errors.New("db down"))
	assert.Len(t, got, 2, "non-image path keeps fail-open on metadata query error")
}

func TestFilterAbilitiesByChannelMetadata_MissingMetadataFailsClosedForSyncImage(t *testing.T) {
	withStubSyncImageEligibility(t)
	chSync := &Channel{Id: 10, Type: constant.ChannelTypeOpenAI}
	// Ability 99 has no channel row (metadata missing); channel 10 is present
	// and sync-capable. The missing-metadata ability must be dropped for a sync
	// image path — treating unknown as sync-capable would re-open the type-59 bug.
	abilities := []Ability{{ChannelId: 10}, {ChannelId: 99}}
	got := filterAbilitiesByChannelMetadata(abilities, "/v1/images/generations", "gpt-image-2", []*Channel{chSync}, nil)
	assert.Len(t, got, 1, "missing-metadata ability must be dropped for sync image")
	assert.Equal(t, 10, got[0].ChannelId, "only the present sync-capable channel remains")
}

func TestFilterAbilitiesByChannelMetadata_MissingMetadataKeptForNonImage(t *testing.T) {
	chSync := &Channel{Id: 10, Type: constant.ChannelTypeOpenAI}
	abilities := []Ability{{ChannelId: 10}, {ChannelId: 99}}
	got := filterAbilitiesByChannelMetadata(abilities, "/v1/chat/completions", "gpt-4o", []*Channel{chSync}, nil)
	assert.Len(t, got, 2, "non-image path keeps the missing-metadata ability (historical behavior)")
}
