package service

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WIS-580: the image-adapter capability registry (imageAdapterRegistry) is the
// single source of truth for which channel/api types can serve a SYNCHRONOUS
// image request via relay.GetAdaptor. ApiNebula (type-59 / APITypeApiNebula) is
// async-only — it has no sync adaptor entry — so it must be reported ineligible
// for sync image selection; OpenAI (type-1) supports sync and is eligible. A
// channel type that is not an image-task adapter at all is left to the normal
// sync relay path, so it must NOT be excluded by this rule (otherwise we'd break
// every non-image chat/embedding channel).

func TestChannelSupportsSyncImage(t *testing.T) {
	assert.True(t, ChannelSupportsSyncImage(constant.ChannelTypeOpenAI),
		"OpenAI has a sync image adaptor -> eligible")
	assert.False(t, ChannelSupportsSyncImage(constant.ChannelTypeApiNebula),
		"ApiNebula is async-only -> not sync-eligible (WIS-580 root cause)")
	assert.True(t, ChannelSupportsSyncImage(constant.ChannelTypeAnthropic),
		"non-image channel type is handled by normal relay, not excluded")
	assert.True(t, ChannelSupportsSyncImage(constant.ChannelTypeAdvancedCustom),
		"Advanced Custom is path-matched separately, not excluded here")
}

func TestApiTypeSupportsSyncImage(t *testing.T) {
	// The dispatch-time (方案 1) guard checks the resolved apiType directly.
	assert.True(t, ApiTypeSupportsSyncImage(constant.APITypeOpenAI))
	assert.False(t, ApiTypeSupportsSyncImage(constant.APITypeApiNebula),
		"async-only apiType must be rejected at ImageHelper dispatch (WIS-580 方案 1)")
	// An apiType with no image-task adapter registration is served by the normal
	// sync relay (GetAdaptor has its case), so it is not rejected by this guard.
	assert.True(t, ApiTypeSupportsSyncImage(constant.APITypeAnthropic))
}

// TestSyncImageEligibilityHookRegistered verifies the service layer injects its
// canonical predicate into model at init, so model's selection path can consult
// the capability truth without importing service (no import cycle). This seam is
// what keeps the capability truth in exactly one place — model never duplicates
// the async-only set.
func TestSyncImageEligibilityHookRegistered(t *testing.T) {
	require.NotNil(t, model.SyncImageChannelEligibility,
		"service init must inject the sync-image eligibility predicate into model")
	assert.False(t, model.SyncImageChannelEligibility(constant.ChannelTypeApiNebula))
	assert.True(t, model.SyncImageChannelEligibility(constant.ChannelTypeOpenAI))
}
