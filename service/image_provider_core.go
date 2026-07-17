package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

// ImageProviderRequest is the normalized, provider-agnostic single-image request
// the processor builds from a frozen execution's RequestData. The adapter maps
// it onto its wire format; the core never re-decodes client JSON on the hot
// path, so a frozen task always submits the same logical request (§7.5).
type ImageProviderRequest struct {
	Operation ImageOperation // generation | edit
	Model     string
	Prompt    string
	Quality   string   // "" when the client omitted quality
	Images    []string // edit input image URLs; empty for generation
}

// ImageAdapterPollStatus is the normalized upstream task status the adapter
// returns from Poll. The adapter folds the provider's own vocabulary onto these
// three values so the processor state machine branches on a closed set.
type ImageAdapterPollStatus string

const (
	// ImagePollRunning means the upstream task is still queued or in progress.
	ImagePollRunning ImageAdapterPollStatus = "running"
	// ImagePollCompleted means the upstream succeeded; DownloadURL is set.
	ImagePollCompleted ImageAdapterPollStatus = "completed"
	// ImagePollFailed means the upstream failed or was cancelled; Message is set.
	ImagePollFailed ImageAdapterPollStatus = "failed"
)

// ImageAdapterPollOutcome is the structured result of one Poll call. On
// completed, DownloadURL points at the upstream's temporary result URL (the
// processor downloads and persists it before finalize). On failed, Message
// carries the provider error text for the audit trail.
type ImageAdapterPollOutcome struct {
	Status      ImageAdapterPollStatus
	DownloadURL string
	Message     string
}

// ErrCancelUnsupported is the sentinel an adapter returns from Cancel when the
// provider exposes no cancel endpoint. The processor treats it as "cancel by
// detached drain": it keeps polling the upstream to a terminal state and never
// forces an upstream cancel (§7.5 cancel absorption). ApiNebula returns this.
var ErrCancelUnsupported = fmt.Errorf("adapter does not support cancel")

// ImageTaskAdapter is the provider execution boundary (§7.5). Each adapter maps
// the normalized request and the frozen channel revision onto one provider's
// submit/poll/cancel protocol and classifies outcomes into typed
// ImageProviderError values. The adapter performs no billing and no cross-channel
// retry; the processor owns the state machine and the billing aggregate owns the
// funds. credential is the channel key resolved from the revision's
// CredentialRef at runtime, so a key rotation takes effect without re-freezing.
type ImageTaskAdapter interface {
	// Submit creates the upstream task and returns its id. A nil error means
	// accepted: the task id is durable and Poll may proceed.
	Submit(ctx context.Context, rev *model.ChannelRevision, credential string, req ImageProviderRequest) (upstreamTaskID string, err error)
	// Poll reads the upstream task status and folds it onto the normalized
	// outcome. A non-nil error is always a retryable or manual-review poll
	// failure; a completed/failed upstream is reported via the outcome, not an
	// error, so the processor can finalize cleanly.
	Poll(ctx context.Context, rev *model.ChannelRevision, credential string, upstreamTaskID string) (ImageAdapterPollOutcome, error)
	// Cancel requests an upstream cancel. Adapters without a cancel endpoint
	// return ErrCancelUnsupported; the processor then drains by polling.
	Cancel(ctx context.Context, rev *model.ChannelRevision, credential string, upstreamTaskID string) error
}

// imageAdapterImpl pairs an adapter implementation with the version label it
// serves. The version is the same string frozen into ChannelRevision and
// ImageTaskExecution, so the processor dispatches the adapter that a frozen task
// was created against (§7.2): a revision created under apinebula-image-adapter/v1
// is always served by the v1 implementation, never a future incompatible one.
type imageAdapterImpl struct {
	adapter  ImageTaskAdapter
	version  string
}

// imageAdapterImplRegistry is the closed set of adapter implementations, keyed
// by version label. The processor resolves an execution's adapter by its frozen
// AdapterVersion. An unregistered version is fail-closed: the execution routes
// to manual_review rather than running against an unknown implementation.
var imageAdapterImplRegistry = map[string]imageAdapterImpl{}

// registerImageAdapter installs an adapter implementation under its version
// label. Called from init() in each adapter file; re-registering a version
// replaces the implementation (tests rely on this).
func registerImageAdapter(version string, adapter ImageTaskAdapter) {
	imageAdapterImplRegistry[version] = imageAdapterImpl{adapter: adapter, version: version}
}

// ResolveImageTaskAdapter returns the adapter implementation for a frozen
// version label. The bool is false when no implementation is registered for that
// version; the processor treats such an execution as manual_review (§7.2:
// a frozen task must run against the implementation it was created under).
func ResolveImageTaskAdapter(version string) (ImageTaskAdapter, bool) {
	impl, ok := imageAdapterImplRegistry[version]
	if !ok {
		return nil, false
	}
	return impl.adapter, true
}

// ResolveChannelCredential resolves a ChannelRevision.CredentialRef to the
// channel's current key. Per §7.2 the credential is a reference
// ("channel:<id>") so a key rotation takes effect immediately without
// re-freezing every revision. An unrecognized ref shape or a missing channel is
// an error the processor surfaces as manual_review (the frozen config cannot
// authenticate).
func ResolveChannelCredential(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	const prefix = "channel:"
	if !strings.HasPrefix(ref, prefix) {
		return "", fmt.Errorf("credential ref %q is not a channel reference", ref)
	}
	idStr := strings.TrimPrefix(ref, prefix)
	idStr = strings.TrimSpace(idStr)
	if idStr == "" {
		return "", fmt.Errorf("credential ref %q has no channel id", ref)
	}
	var id int
	if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil || id <= 0 {
		return "", fmt.Errorf("credential ref %q has an invalid channel id", ref)
	}
	channel, err := model.GetChannelById(id, true)
	if err != nil {
		return "", fmt.Errorf("resolve credential channel %d: %w", id, err)
	}
	key, _, nErr := channel.GetNextEnabledKey()
	if nErr != nil {
		return "", fmt.Errorf("credential channel %d has no enabled key: %w", id, nErr)
	}
	if key == "" {
		return "", fmt.Errorf("credential channel %d has an empty key", id)
	}
	return key, nil
}

// BuildImageProviderRequest maps the frozen RequestData JSON onto the normalized
// provider request. Generation yields an empty Images slice; edit yields the
// ordered input image URLs. The processor calls this once per submit attempt so
// the frozen task always submits the same logical request.
func BuildImageProviderRequest(operation ImageOperation, requestData []byte) (ImageProviderRequest, error) {
	switch operation {
	case ImageOperationGeneration:
		var req struct {
			Model   string  `json:"model"`
			Prompt  string  `json:"prompt"`
			Quality *string `json:"quality"`
		}
		if err := decodeProviderRequest(requestData, &req); err != nil {
			return ImageProviderRequest{}, err
		}
		return newProviderRequest(ImageOperationGeneration, req.Model, req.Prompt, req.Quality, nil), nil
	case ImageOperationEdit:
		var req struct {
			Model   string `json:"model"`
			Prompt  string `json:"prompt"`
			Quality *string `json:"quality"`
			Images  []struct {
				ImageURL string `json:"image_url"`
			} `json:"images"`
		}
		if err := decodeProviderRequest(requestData, &req); err != nil {
			return ImageProviderRequest{}, err
		}
		urls := make([]string, 0, len(req.Images))
		for _, img := range req.Images {
			urls = append(urls, img.ImageURL)
		}
		return newProviderRequest(ImageOperationEdit, req.Model, req.Prompt, req.Quality, urls), nil
	default:
		return ImageProviderRequest{}, fmt.Errorf("unsupported image operation %q", operation)
	}
}

// decodeProviderRequest is a thin wrapper so BuildImageProviderRequest stays
// focused on field mapping. It uses common.Unmarshal (the AGENTS.md JSON rule).
func decodeProviderRequest(requestData []byte, target any) error {
	if len(requestData) == 0 {
		return fmt.Errorf("image provider request data is empty")
	}
	return common.Unmarshal(requestData, target)
}

// newProviderRequest assembles an immutable ImageProviderRequest, copying the
// images slice so the caller cannot mutate it later. Quality is dereferenced
// only when the client supplied a non-nil pointer.
func newProviderRequest(op ImageOperation, model, prompt string, quality *string, images []string) ImageProviderRequest {
	req := ImageProviderRequest{
		Operation: op,
		Model:     model,
		Prompt:    prompt,
		Images:    append([]string(nil), images...),
	}
	if quality != nil {
		req.Quality = *quality
	}
	return req
}
