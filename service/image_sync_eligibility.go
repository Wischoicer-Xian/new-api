package service

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

// WIS-580: synchronous image relay paths (/v1/images/generations|/edits, served
// by relay.ImageHelper via relay.GetAdaptor) must not be served by channels
// whose image provider is async/task-only — e.g. ChannelTypeApiNebula (type-59),
// which Phase-7 serves exclusively through /v1/image-tasks/* + ChannelRevision.
// Such apiTypes have NO entry in relay.GetAdaptor's switch, so if a sync image
// request selects one, relay.ImageHelper hits GetAdaptor(info.ApiType)==nil and
// returns "invalid api type: 36" HTTP 500.
//
// The capability truth — which apiType supports which image execution modes —
// already lives in imageAdapterRegistry (service.image_adapter_capabilities).
// The model selection layer cannot import service (import cycle), so it exposes
// a hook (model.SyncImageChannelEligibility) that this package fills in init().
// That keeps the truth in exactly one place; model never duplicates the
// async-only set.

// ApiTypeSupportsSyncImage reports whether the given apiType can serve a
// synchronous image request through relay.GetAdaptor. It is the dispatch-time
// (方案 1) guard used by relay.ImageHelper, and the primitive on which
// ChannelSupportsSyncImage builds.
//
// A registered image adapter is sync-eligible iff it supports sync execution for
// at least one image operation (generation or edit). An apiType that is not an
// image-task adapter at all is left to the normal sync relay (GetAdaptor has its
// own case for it), so it is NOT rejected here.
func ApiTypeSupportsSyncImage(apiType int) bool {
	caps, ok := ImageAdapterCapabilities(apiType)
	if !ok {
		return true
	}
	for _, op := range []ImageOperation{ImageOperationGeneration, ImageOperationEdit} {
		for _, mode := range caps.ImageTaskExecutionSupport(op) {
			if mode == ImageExecutionSync {
				return true
			}
		}
	}
	return false
}

// ChannelSupportsSyncImage reports whether a channel of the given type can serve
// a synchronous image request. It resolves the channel type to its apiType and
// delegates to ApiTypeSupportsSyncImage.
func ChannelSupportsSyncImage(channelType int) bool {
	apiType, _ := common.ChannelType2APIType(channelType)
	return ApiTypeSupportsSyncImage(apiType)
}

// init injects the canonical sync-image eligibility predicate into the model
// selection layer, so model's candidate filter can apply the capability truth
// without importing service. Runs once at process start, before any request.
func init() {
	model.SyncImageChannelEligibility = ChannelSupportsSyncImage
}
