package service

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

// WIS-580: synchronous image relay paths (/v1/images/generations|/edits, served
// by relay.ImageHelper via relay.GetAdaptor) must not be served by channels
// whose image provider is async/task-only — e.g. ChannelTypeApiNebula (type 61),
// which Phase-7 serves exclusively through /v1/image-tasks/* + ChannelRevision.
// Such apiTypes have NO entry in relay.GetAdaptor's switch, so if a sync image
// request selects one, relay.ImageHelper hits GetAdaptor(info.ApiType)==nil and
// returns "invalid api type: 36" HTTP 500.
//
// P2 (记星 review round 2): eligibility is OPERATION-granular. The check is per
// (apiType, operation): a channel that supports sync generation but async edit
// must still be rejected for a sync /v1/images/edits request. The capability
// truth lives in imageAdapterRegistry; the operation is derived from the request
// path (single prefix list owned by model). model cannot import service (import
// cycle), so it exposes a hook (model.SyncImageChannelEligibility) that this
// package fills in init() — one source of truth, operation-granular, no duplication.

// ApiTypeSupportsSyncImageForOp reports whether the given apiType can serve a
// synchronous image request for the given operation through relay.GetAdaptor.
//
// A registered image adapter is sync-eligible for an operation iff it lists sync
// execution for that operation (capsSupports). An apiType that is not an image-task
// adapter at all is left to the normal sync relay (GetAdaptor has its own case),
// so it is NOT rejected here. An unknown operation fails closed (return false).
func ApiTypeSupportsSyncImageForOp(apiType int, op model.SyncImageOperation) bool {
	caps, ok := ImageAdapterCapabilities(apiType)
	if !ok {
		return true
	}
	sop, known := opToServiceOp(op)
	if !known {
		return false // sync-image path but op underrivable -> fail-close
	}
	return capsSupports(caps, sop, ImageExecutionSync)
}

// opToServiceOp maps model's path-derived operation tag to the image-task
// ImageOperation consumed by the capability registry. model owns the tag so the
// path-prefix list stays in one place; this mapping is the only seam.
func opToServiceOp(op model.SyncImageOperation) (ImageOperation, bool) {
	switch op {
	case model.SyncImageOpGeneration:
		return ImageOperationGeneration, true
	case model.SyncImageOpEdit:
		return ImageOperationEdit, true
	}
	return "", false
}

// ApiTypeSupportsSyncImageForPath derives the operation from requestPath and
// delegates to ApiTypeSupportsSyncImageForOp. Non-sync-image paths are not
// governed by this rule (return true).
func ApiTypeSupportsSyncImageForPath(apiType int, requestPath string) bool {
	if !model.IsSyncImagePath(requestPath) {
		return true
	}
	return ApiTypeSupportsSyncImageForOp(apiType, model.SyncImageOperationFromPath(requestPath))
}

// ChannelSupportsSyncImageForOp / ForPath are the channel-type-facing wrappers,
// resolving the channel type to its apiType first. Used by the middleware
// affinity path and registered as the model eligibility hook.
func ChannelSupportsSyncImageForOp(channelType int, op model.SyncImageOperation) bool {
	apiType, _ := common.ChannelType2APIType(channelType)
	return ApiTypeSupportsSyncImageForOp(apiType, op)
}

func ChannelSupportsSyncImageForPath(channelType int, requestPath string) bool {
	if !model.IsSyncImagePath(requestPath) {
		return true
	}
	return ChannelSupportsSyncImageForOp(channelType, model.SyncImageOperationFromPath(requestPath))
}

// init injects the canonical, operation-granular sync-image eligibility predicate
// into the model selection layer, so model's candidate filter can apply the
// capability truth without importing service. Runs once at process start, before
// any request.
func init() {
	model.SyncImageChannelEligibility = ChannelSupportsSyncImageForOp
}
