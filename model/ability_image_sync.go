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
// Which channel/api types are async-only is a capability fact owned by the service
// layer (imageAdapterRegistry). model cannot import service (relay imports model
// -> import cycle), so service injects that truth through SyncImageChannelEligibility
// at init time. When the hook is unset (e.g. model-only tests, or before service
// init), no channel is excluded by this rule — the default is permissive, never a
// silent exclusion.
//
// P2 (记星 review round 2): the check is OPERATION-granular. The operation is
// derived from the request path here (single prefix list), so service can decide
// per (apiType, generation|edit) and a mixed adapter cannot sneak an async edit
// onto the sync /v1/images/edits path.

// SyncImageOperation is the image operation derived from a sync relay path. It is
// the model-side tag passed to the injected eligibility predicate; service maps it
// to its own ImageOperation so the path-prefix list lives in exactly one place.
type SyncImageOperation int

const (
	// SyncImageOpUnknown is the zero value; the eligibility check fails closed on it.
	SyncImageOpUnknown SyncImageOperation = iota
	SyncImageOpGeneration
	SyncImageOpEdit
)

// SyncImageChannelEligibility, when set, reports whether a channel of the given
// type may serve a SYNCHRONOUS image request for the given operation. It is
// injected by the service layer (owner of the image-adapter capability truth) to
// avoid a model->service import cycle. When nil, excludeChannelForSyncImage
// returns false (permissive) so the rule never silently drops a channel.
var SyncImageChannelEligibility func(channelType int, op SyncImageOperation) bool

// IsSyncImagePath reports whether requestPath is a synchronous OpenAI-compatible
// image relay path — the routes served by relay.ImageHelper via GetAdaptor. Async
// image-task routes (/v1/image-tasks/*) are served via ChannelRevision and never
// reach the sync selection path.
func IsSyncImagePath(requestPath string) bool {
	return strings.HasPrefix(requestPath, "/v1/images/generations") ||
		strings.HasPrefix(requestPath, "/v1/images/edits") ||
		strings.HasPrefix(requestPath, "/v1/edits")
}

// SyncImageOperationFromPath derives the image operation from a sync image relay
// path. Returns SyncImageOpUnknown for non-image paths (callers gate on
// IsSyncImagePath first, so within the gate the result is always generation/edit).
func SyncImageOperationFromPath(requestPath string) SyncImageOperation {
	switch {
	case strings.HasPrefix(requestPath, "/v1/images/generations"):
		return SyncImageOpGeneration
	case strings.HasPrefix(requestPath, "/v1/images/edits"), strings.HasPrefix(requestPath, "/v1/edits"):
		return SyncImageOpEdit
	}
	return SyncImageOpUnknown
}

// excludeChannelForSyncImage reports whether the channel of the given type must
// be excluded from candidate selection for the given request path: true when the
// path is a synchronous image path and the channel type is not sync-capable for
// that path's operation (per the injected predicate). Permissive when the path is
// not a sync image path or the hook is unset.
func excludeChannelForSyncImage(requestPath string, channelType int) bool {
	if !IsSyncImagePath(requestPath) {
		return false
	}
	if SyncImageChannelEligibility == nil {
		return false
	}
	return !SyncImageChannelEligibility(channelType, SyncImageOperationFromPath(requestPath))
}
