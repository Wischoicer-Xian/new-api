package service

import (
	"strconv"

	"github.com/QuantumNous/new-api/constant"
)

// staticImageAdapterCaps is a declarative ImageTaskAdapterCapabilities: the
// support set and per-operation defaults are fixed at registration time so the
// admin layer and the image task candidate-pool filter share one source of
// truth. An operation with no entry returns an empty support slice, which the
// resolver treats as fail closed for that operation.
type staticImageAdapterCaps struct {
	support  map[ImageOperation][]ImageExecutionMode
	defaults map[ImageOperation]ImageExecutionMode
}

func (s staticImageAdapterCaps) ImageTaskExecutionSupport(op ImageOperation) []ImageExecutionMode {
	return s.support[op]
}

func (s staticImageAdapterCaps) ImageTaskDefaultExecution(op ImageOperation) (ImageExecutionMode, bool) {
	mode, ok := s.defaults[op]
	return mode, ok
}

// imageAdapterRegistry is the closed set of channel adapters that opt into the
// image task subsystem. An apiType absent from this map is treated as not
// supporting image tasks at all: channels of that type cannot be configured
// for image execution and never enter the image task candidate pool. This is
// what prevents guessing sync/async behavior from model names or response
// shapes at runtime.
//
// Only adapters whose image protocol is actually understood by new-api are
// registered, and each declares the hard boundary the channel configuration is
// validated against. ApiNebula's async_task support is added here when its
// adapter lands.
var imageAdapterRegistry = map[int]staticImageAdapterCaps{
	// OpenAI-compatible adapter: both generation and edit are synchronous.
	constant.APITypeOpenAI: {
		support: map[ImageOperation][]ImageExecutionMode{
			ImageOperationGeneration: {ImageExecutionSync},
			ImageOperationEdit:       {ImageExecutionSync},
		},
		defaults: map[ImageOperation]ImageExecutionMode{
			ImageOperationGeneration: ImageExecutionSync,
			ImageOperationEdit:       ImageExecutionSync,
		},
	},
}

// ImageAdapterCapabilities resolves the image task execution boundary for a
// channel API type. The bool is false when the adapter has not opted into the
// image task subsystem; callers must treat such channels as ineligible rather
// than guessing a default mode.
func ImageAdapterCapabilities(apiType int) (ImageTaskAdapterCapabilities, bool) {
	caps, ok := imageAdapterRegistry[apiType]
	if !ok {
		return nil, false
	}
	return caps, true
}

// ImageAdapterVersion returns a stable version label for the adapter serving
// apiType. It is frozen into channel revisions so a frozen image task can
// detect whether it runs against the adapter implementation it was created
// under. The label is intentionally opaque and string-stable across builds.
func ImageAdapterVersion(apiType int) string {
	return "apitype:" + strconv.Itoa(apiType)
}

// ImageCapabilityPreviewEntry is one resolved operation+model on a channel,
// surfaced to the admin UI so a misconfiguration is explainable. Ok is false
// when the configured mode lies outside the adapter support set, in which case
// the channel is fail-closed for that resolution rather than silently degrading.
type ImageCapabilityPreviewEntry struct {
	Operation ImageOperation        `json:"operation"`
	Model     string                `json:"model,omitempty"`
	Mode      ImageExecutionMode    `json:"mode,omitempty"`
	Source    ImageCapabilitySource `json:"source,omitempty"`
	Ok        bool                  `json:"ok"`
}

// imageOperations is the fixed operation set image tasks support, in a stable
// order. Centralizing it keeps the preview and any future iteration over both
// operations from drifting apart or silently dropping one.
var imageOperations = []ImageOperation{ImageOperationGeneration, ImageOperationEdit}

// PreviewImageChannelExecution resolves the execution mode for the channel-wide
// operation defaults and every configured model override, so the admin UI can
// render the effective mode, its source, and any fail-closed entry. models is
// the channel's upstream model list; only models that carry an override (or the
// default resolution when no override exists) are reported, keeping the preview
// bounded to what an operator can actually change.
func PreviewImageChannelExecution(caps ImageTaskAdapterCapabilities, c ImageChannelExecutionConfig, models []string) []ImageCapabilityPreviewEntry {
	if caps == nil {
		return nil
	}
	preview := make([]ImageCapabilityPreviewEntry, 0, 2+len(models)*2)
	// Channel-wide default resolution: one entry per operation the adapter
	// supports, reflecting the precedence model override -> channel default ->
	// adapter default for an arbitrary (non-overridden) model.
	for _, op := range imageOperations {
		if len(caps.ImageTaskExecutionSupport(op)) == 0 {
			continue
		}
		res, ok := ResolveImageExecution(caps, c, op, "")
		preview = append(preview, ImageCapabilityPreviewEntry{
			Operation: op,
			Mode:      res.Mode,
			Source:    res.Source,
			Ok:        ok,
		})
	}
	// Per-model overrides: report each model+operation that has an explicit
	// override, so an admin sees exactly the rows they configured.
	for _, model := range models {
		perOp, ok := c.Models[model]
		if !ok {
			continue
		}
		for _, op := range imageOperations {
			if _, configured := perOp[op]; !configured {
				continue
			}
			res, ok := ResolveImageExecution(caps, c, op, model)
			preview = append(preview, ImageCapabilityPreviewEntry{
				Operation: op,
				Model:     model,
				Mode:      res.Mode,
				Source:    res.Source,
				Ok:        ok,
			})
		}
	}
	return preview
}
