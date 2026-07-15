package service

import "fmt"

// ImageOperation distinguishes the two image task operations a channel can
// serve. It is orthogonal to the existing constant.EndpointType: endpoint
// type expresses whether a channel speaks an image protocol at all, while
// ImageOperation separates generation from edit so they can resolve to
// different execution modes on the same channel.
type ImageOperation string

const (
	ImageOperationGeneration ImageOperation = "generation"
	ImageOperationEdit       ImageOperation = "edit"
)

// ImageExecutionMode declares how a single image is produced for one task.
// sync means the provider call returns the image inline; async_task means
// the adapter submits a job and polls for completion.
type ImageExecutionMode string

const (
	ImageExecutionSync      ImageExecutionMode = "sync"
	ImageExecutionAsyncTask ImageExecutionMode = "async_task"
)

// ImageTaskAdapterCapabilities declares the compile-time hard boundary: the
// set of execution modes an adapter actually implements for each operation.
// Channel configuration may narrow or override this set but can never enable
// a mode the adapter does not implement, which is what prevents guessing
// sync/async behavior from model names or response shapes at runtime.
type ImageTaskAdapterCapabilities interface {
	ImageTaskExecutionSupport(op ImageOperation) []ImageExecutionMode
	ImageTaskDefaultExecution(op ImageOperation) (ImageExecutionMode, bool)
}

// ImageChannelExecutionConfig is the per-channel execution configuration.
// Defaults maps an operation to its channel-wide execution mode; Models
// overrides it for a specific upstream model. An unset entry means "fall
// through to the next precedence level", never "inherit an unrelated value".
type ImageChannelExecutionConfig struct {
	Defaults map[ImageOperation]ImageExecutionMode
	Models   map[string]map[ImageOperation]ImageExecutionMode
}

// ImageCapabilitySource records which precedence layer supplied the resolved
// mode, surfaced to admins so a misconfiguration is explainable.
type ImageCapabilitySource string

const (
	ImageCapabilitySourceModelOverride  ImageCapabilitySource = "model_override"
	ImageCapabilitySourceChannelDefault ImageCapabilitySource = "channel_default"
	ImageCapabilitySourceAdapterDefault ImageCapabilitySource = "adapter_default"
)

// ImageCapabilityResolution is the outcome of resolving one operation+model
// on one channel. ok is false when the resolved mode lies outside the
// adapter support set, in which case the caller must treat the channel as
// ineligible (fail closed) rather than guessing.
type ImageCapabilityResolution struct {
	Mode   ImageExecutionMode
	Source ImageCapabilitySource
}

// ResolveImageExecution resolves the execution mode for one operation+model
// following the documented precedence:
//
//	model override -> channel default -> adapter default
//
// and then validates the result against the adapter support set. A mode
// configured at a higher precedence level that the adapter does not support
// fails closed instead of silently degrading to a lower level: a channel that
// thinks it runs async for a sync-only adapter is a misconfiguration to
// surface, not a behavior to paper over.
func ResolveImageExecution(caps ImageTaskAdapterCapabilities, c ImageChannelExecutionConfig, op ImageOperation, model string) (ImageCapabilityResolution, bool) {
	support := caps.ImageTaskExecutionSupport(op)
	if len(support) == 0 {
		return ImageCapabilityResolution{}, false
	}
	supported := make(map[ImageExecutionMode]struct{}, len(support))
	for _, m := range support {
		supported[m] = struct{}{}
	}

	if perModel, ok := c.Models[model]; ok {
		if mode, ok := perModel[op]; ok {
			return finalize(mode, supported, ImageCapabilitySourceModelOverride)
		}
	}
	if mode, ok := c.Defaults[op]; ok {
		return finalize(mode, supported, ImageCapabilitySourceChannelDefault)
	}
	mode, ok := caps.ImageTaskDefaultExecution(op)
	if !ok {
		return ImageCapabilityResolution{}, false
	}
	return finalize(mode, supported, ImageCapabilitySourceAdapterDefault)
}

func finalize(mode ImageExecutionMode, supported map[ImageExecutionMode]struct{}, source ImageCapabilitySource) (ImageCapabilityResolution, bool) {
	if _, ok := supported[mode]; !ok {
		return ImageCapabilityResolution{}, false
	}
	return ImageCapabilityResolution{Mode: mode, Source: source}, true
}

// ValidateImageChannelExecutionConfig statically validates that every
// configured mode — both the operation defaults and all model overrides —
// falls inside the adapter support set. Called when an admin saves channel
// configuration and as a defense-in-depth check at load time so that an
// invalid channel never silently enters the image task candidate pool.
func ValidateImageChannelExecutionConfig(caps ImageTaskAdapterCapabilities, c ImageChannelExecutionConfig) error {
	for op, mode := range c.Defaults {
		if !capsSupports(caps, op, mode) {
			return fmt.Errorf("image execution default %q for operation %q is not supported by this adapter", mode, op)
		}
	}
	for model, perOp := range c.Models {
		for op, mode := range perOp {
			if !capsSupports(caps, op, mode) {
				return fmt.Errorf("image execution override %q for model %q operation %q is not supported by this adapter", mode, model, op)
			}
		}
	}
	return nil
}

func capsSupports(caps ImageTaskAdapterCapabilities, op ImageOperation, mode ImageExecutionMode) bool {
	for _, m := range caps.ImageTaskExecutionSupport(op) {
		if m == mode {
			return true
		}
	}
	return false
}
