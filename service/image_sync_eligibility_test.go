package service

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WIS-580 P2 (记星 review round 2): sync-image eligibility must be OPERATION
// granular, not "generation OR edit supports sync -> pass whole channel". A
// hypothetical mixed adapter (sync generation, async edit) must be rejected for a
// sync /v1/images/edits request even though it passes a /v1/images/generations
// check. The capability truth stays in imageAdapterRegistry; op is derived from
// the request path (single prefix list lives in model).

func TestApiTypeSupportsSyncImageForPath(t *testing.T) {
	// OpenAI supports sync for both operations on both sync image paths.
	assert.True(t, ApiTypeSupportsSyncImageForPath(constant.APITypeOpenAI, "/v1/images/generations"))
	assert.True(t, ApiTypeSupportsSyncImageForPath(constant.APITypeOpenAI, "/v1/images/edits"))
	// ApiNebula is async-only for BOTH operations on BOTH paths.
	assert.False(t, ApiTypeSupportsSyncImageForPath(constant.APITypeApiNebula, "/v1/images/generations"),
		"async-only apiType must be rejected for sync generation (WIS-580 root cause)")
	assert.False(t, ApiTypeSupportsSyncImageForPath(constant.APITypeApiNebula, "/v1/images/edits"),
		"async-only apiType must be rejected for sync edit too")
	// Non-image apiType (its own GetAdaptor case) passes.
	assert.True(t, ApiTypeSupportsSyncImageForPath(constant.APITypeAnthropic, "/v1/images/generations"))
	// Non-sync-image path is not governed by this rule.
	assert.True(t, ApiTypeSupportsSyncImageForPath(constant.APITypeApiNebula, "/v1/chat/completions"))
}

func TestChannelSupportsSyncImageForPath(t *testing.T) {
	assert.True(t, ChannelSupportsSyncImageForPath(constant.ChannelTypeOpenAI, "/v1/images/edits"))
	assert.False(t, ChannelSupportsSyncImageForPath(constant.ChannelTypeApiNebula, "/v1/images/edits"))
	assert.False(t, ChannelSupportsSyncImageForPath(constant.ChannelTypeApiNebula, "/v1/images/generations"))
}

// TestCapsSupportsSyncOperationGranularity is the mixed-adapter defense: a caps
// that supports sync generation but async edit must pass generation and fail edit.
// This is the primitive that keeps an async edit off the sync /v1/images/edits path.
func TestCapsSupportsSyncOperationGranularity(t *testing.T) {
	mixed := staticImageAdapterCaps{
		support: map[ImageOperation][]ImageExecutionMode{
			ImageOperationGeneration: {ImageExecutionSync},
			ImageOperationEdit:       {ImageExecutionAsyncTask},
		},
	}
	assert.True(t, capsSupports(mixed, ImageOperationGeneration, ImageExecutionSync),
		"sync generation must pass")
	assert.False(t, capsSupports(mixed, ImageOperationEdit, ImageExecutionSync),
		"async edit must NOT pass the sync check (operation granularity)")
}

// TestSyncImageEligibilityHookRegistered verifies service injects its canonical,
// operation-granular predicate into model at init (no model->service import cycle,
// single source of truth).
func TestSyncImageEligibilityHookRegistered(t *testing.T) {
	require.NotNil(t, model.SyncImageChannelEligibility,
		"service init must inject the sync-image eligibility predicate into model")
	assert.False(t, model.SyncImageChannelEligibility(constant.ChannelTypeApiNebula, model.SyncImageOpGeneration))
	assert.False(t, model.SyncImageChannelEligibility(constant.ChannelTypeApiNebula, model.SyncImageOpEdit))
	assert.True(t, model.SyncImageChannelEligibility(constant.ChannelTypeOpenAI, model.SyncImageOpGeneration))
	assert.True(t, model.SyncImageChannelEligibility(constant.ChannelTypeOpenAI, model.SyncImageOpEdit))
}
