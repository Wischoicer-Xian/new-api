package model

import "strings"

// WIS-580: synchronous image relay paths (/v1/images/generations, /v1/images/edits,
// /v1/edits — served by relay.ImageHelper via relay.GetAdaptor) must NOT select
// channels whose image provider is async/task-only — e.g. ChannelTypeApiNebula
// (type-59), which Phase-7 serves exclusively via /v1/image-tasks/* +
// ChannelRevision. Those apiTypes have no entry in relay.GetAdaptor's switch, so
// if one is selected for a sync image request, relay.ImageHelper hits
// GetAdaptor(info.ApiType)==nil and returns "invalid api type" 500.
//
// Which channel types are async-only is a capability fact owned by the service
// layer (imageAdapterRegistry). model cannot import service (relay imports model
// -> import cycle), so service injects that truth through SyncImageChannelEligibility
// at init time. When the hook is unset (e.g. model-only tests, or before service
// init), no channel is excluded by this rule — the default is permissive, never a
// silent exclusion.

// SyncImageChannelEligibility, when set, reports whether a channel of the given
// type may serve a SYNCHRONOUS image request. It is injected by the service layer
// (owner of the image-adapter capability truth) to avoid a model->service import
// cycle. When nil, channelTypeSupportsSyncImage returns true (permissive) so the
// rule never silently drops a channel.
var SyncImageChannelEligibility func(channelType int) bool

// IsSyncImagePath reports whether requestPath is a synchronous OpenAI-compatible
// image relay path — the routes served by relay.ImageHelper via GetAdaptor. Async
// image-task routes (/v1/image-tasks/*) are served via ChannelRevision and never
// reach the sync selection path.
func IsSyncImagePath(requestPath string) bool {
	return strings.HasPrefix(requestPath, "/v1/images/generations") ||
		strings.HasPrefix(requestPath, "/v1/images/edits") ||
		strings.HasPrefix(requestPath, "/v1/edits")
}

// channelTypeSupportsSyncImage reports whether a channel of the given type can
// serve a synchronous image request. It defers to the injected capability
// predicate when present; otherwise it is permissive (returns true).
func channelTypeSupportsSyncImage(channelType int) bool {
	if SyncImageChannelEligibility == nil {
		return true
	}
	return SyncImageChannelEligibility(channelType)
}

// excludeChannelForSyncImage reports whether the channel of the given type must
// be excluded from candidate selection for the given request path: true when the
// path is a synchronous image path and the channel type is not sync-capable.
func excludeChannelForSyncImage(requestPath string, channelType int) bool {
	return IsSyncImagePath(requestPath) && !channelTypeSupportsSyncImage(channelType)
}
