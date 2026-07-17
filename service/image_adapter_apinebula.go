package service

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
)

// apinebulaSubmitResponse is the subset of the ApiNebula submit response the
// adapter consumes. task_id is preferred; id is the fallback handle.
type apinebulaSubmitResponse struct {
	TaskID string              `json:"task_id"`
	ID     string              `json:"id"`
	Status string              `json:"status"`
	Error  *apinebulaErrorBody `json:"error"`
}

// apinebulaPollResponse is the subset of the GET ?detail=true response the
// adapter consumes. detail.data[0].download_url is the temporary upstream result
// URL on completed; error.message is the failure text on failed.
type apinebulaPollResponse struct {
	Status string               `json:"status"`
	Detail *apinebulaPollDetail `json:"detail"`
	Error  *apinebulaErrorBody  `json:"error"`
}

type apinebulaPollDetail struct {
	Data []apinebulaPollResult `json:"data"`
}

type apinebulaPollResult struct {
	DownloadURL    string `json:"download_url"`
	ResponseFormat string `json:"response_format"`
}

type apinebulaErrorBody struct {
	Message string `json:"message"`
}

// apinebulaAdapter implements ImageTaskAdapter for the ApiNebula async image
// provider. Generation and edit are both async_task: submit returns a task id
// and the processor polls GET /v1/image-tasks/{id}?detail=true for completion.
// ApiNebula exposes no cancel endpoint, so Cancel returns ErrCancelUnsupported
// and the processor drains by polling (§7.5 cancel absorption). No Idempotency-
// Key is sent upstream (§7.6: new-api owns the idempotency namespace), so the
// processor never re-submits after an unknowable outcome.
type apinebulaAdapter struct{}

// apinebulaHTTPClient resolves the HTTP client for one request from the frozen
// proxy. Production uses the proxy-aware relay client; tests override this seam
// to route requests at an httptest server. It is NOT the SSRF-protected client:
// provider endpoints are operator-managed deployment targets that may legitimately
// live on private networks or behind a proxy (service/http_client.go).
var apinebulaHTTPClient = defaultApinebulaHTTPClient

func defaultApinebulaHTTPClient(proxyURL string) (*http.Client, error) {
	return GetHttpClientWithProxy(proxyURL)
}

func init() {
	registerImageAdapter(constant.ImageAdapterVersionApiNebula, &apinebulaAdapter{})
}

// apiNebulaPathFor returns the upstream submit path for one operation.
func apiNebulaPathFor(operation ImageOperation) (string, error) {
	switch operation {
	case ImageOperationGeneration:
		return "/v1/image-tasks/generations", nil
	case ImageOperationEdit:
		return "/v1/image-tasks/edits", nil
	default:
		return "", fmt.Errorf("apinebula: unsupported operation %q", operation)
	}
}

func (a *apinebulaAdapter) Submit(ctx context.Context, rev *model.ChannelRevision, credential string, req ImageProviderRequest) (string, error) {
	path, err := apiNebulaPathFor(req.Operation)
	if err != nil {
		return "", err
	}
	body, err := buildApinebulaSubmitBody(req)
	if err != nil {
		return "", err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, joinApinebulaURL(rev.Endpoint, path), bytes.NewReader(body))
	if err != nil {
		return "", networkSubmitError(ImageProviderStageSubmit, err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+credential)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := doApinebula(ctx, rev, httpReq, ImageProviderStageSubmit)
	if err != nil {
		return "", err
	}
	respBody, netErr := readBodyClose(resp, func(e error) *ImageProviderError {
		return networkSubmitError(ImageProviderStageSubmit, e)
	})
	if netErr != nil {
		return "", netErr
	}
	var parsed apinebulaSubmitResponse
	if err := common.Unmarshal(respBody, &parsed); err != nil {
		return "", &ImageProviderError{
			Kind: ImageErrSubmissionUnknown, Stage: ImageProviderStageSubmit,
			Status: resp.StatusCode, UpstreamMessage: "unparseable submit response",
			Err: err,
		}
	}
	if classified := classifySubmitHTTP(resp.StatusCode, apinebulaTaskIDPresent(parsed.TaskID, parsed.ID), apinebulaErrorMessage(parsed.Error)); classified != nil {
		return "", classified
	}
	return apinebulaChooseTaskID(parsed.TaskID, parsed.ID), nil
}

func (a *apinebulaAdapter) Poll(ctx context.Context, rev *model.ChannelRevision, credential string, upstreamTaskID string) (ImageAdapterPollOutcome, error) {
	if upstreamTaskID == "" {
		return ImageAdapterPollOutcome{}, &ImageProviderError{
			Kind: ImageErrManualReview, Stage: ImageProviderStagePoll,
			UpstreamMessage: "poll attempted with empty upstream task id",
		}
	}
	pollURL := joinApinebulaURL(rev.Endpoint, fmt.Sprintf("/v1/image-tasks/%s?detail=true", url.PathEscape(upstreamTaskID)))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
	if err != nil {
		return ImageAdapterPollOutcome{}, networkPollError(err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+credential)
	httpReq.Header.Set("Accept", "application/json")

	resp, err := doApinebula(ctx, rev, httpReq, ImageProviderStagePoll)
	if err != nil {
		return ImageAdapterPollOutcome{}, err
	}
	respBody, netErr := readBodyClose(resp, networkPollError)
	if netErr != nil {
		return ImageAdapterPollOutcome{}, netErr
	}
	var parsed apinebulaPollResponse
	if err := common.Unmarshal(respBody, &parsed); err != nil {
		return ImageAdapterPollOutcome{}, &ImageProviderError{
			Kind: ImageErrRetryablePoll, Stage: ImageProviderStagePoll,
			Status: resp.StatusCode, UpstreamMessage: "unparseable poll response",
			Err: err,
		}
	}
	if classified := classifyPollHTTP(resp.StatusCode, apinebulaErrorMessage(parsed.Error)); classified != nil {
		return ImageAdapterPollOutcome{}, classified
	}
	if normalizeApinebulaStatus(parsed.Status) == apinebulaStatusUnknown {
		return ImageAdapterPollOutcome{}, &ImageProviderError{
			Kind: ImageErrManualReview, Stage: ImageProviderStagePoll,
			Status: resp.StatusCode, UpstreamMessage: fmt.Sprintf("unknown provider task status %q", parsed.Status),
		}
	}
	return foldApinebulaStatus(parsed), nil
}

func (a *apinebulaAdapter) Cancel(ctx context.Context, rev *model.ChannelRevision, credential string, upstreamTaskID string) error {
	// ApiNebula documents only submit and poll; there is no cancel endpoint.
	// Returning the sentinel makes the processor drain by polling (§7.5).
	return ErrCancelUnsupported
}

// doApinebula resolves the HTTP client from the frozen proxy and executes the
// request, mapping transport errors onto the stage's network classifier.
func doApinebula(ctx context.Context, rev *model.ChannelRevision, httpReq *http.Request, stage ImageProviderStage) (*http.Response, error) {
	client, err := apinebulaHTTPClient(rev.Proxy)
	if err != nil {
		if stage == ImageProviderStagePoll {
			return nil, networkPollError(err)
		}
		return nil, networkSubmitError(stage, err)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		if stage == ImageProviderStagePoll {
			return nil, networkPollError(err)
		}
		return nil, networkSubmitError(stage, err)
	}
	return resp, nil
}

// foldApinebulaStatus maps the ApiNebula status vocabulary onto the normalized
// poll outcome. Non-terminal upstream states fold onto running; succeeded/
// completed onto completed (with the first download_url); failed/cancelled onto
// failed (with the error message).
func foldApinebulaStatus(parsed apinebulaPollResponse) ImageAdapterPollOutcome {
	switch normalizeApinebulaStatus(parsed.Status) {
	case apinebulaStatusCompleted:
		out := ImageAdapterPollOutcome{Status: ImagePollCompleted}
		if parsed.Detail != nil && len(parsed.Detail.Data) > 0 {
			out.DownloadURL = parsed.Detail.Data[0].DownloadURL
		}
		return out
	case apinebulaStatusFailed:
		return ImageAdapterPollOutcome{Status: ImagePollFailed, Message: apinebulaErrorMessage(parsed.Error)}
	default:
		return ImageAdapterPollOutcome{Status: ImagePollRunning}
	}
}

type apinebulaStatusKind int

const (
	apinebulaStatusUnknown apinebulaStatusKind = iota
	apinebulaStatusRunning
	apinebulaStatusCompleted
	apinebulaStatusFailed
)

// normalizeApinebulaStatus lower-cases and trims the raw status, then folds the
// documented vocabulary and its attested synonyms onto one of four kinds. An
// unrecognized value stays unknown so Poll can fail closed into manual_review
// rather than polling an unsupported protocol state forever.
func normalizeApinebulaStatus(status string) apinebulaStatusKind {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "queued", "waiting", "running", "in_progress":
		return apinebulaStatusRunning
	case "succeeded", "completed":
		return apinebulaStatusCompleted
	case "failed", "cancelled", "canceled":
		return apinebulaStatusFailed
	default:
		return apinebulaStatusUnknown
	}
}

func apinebulaErrorMessage(errObj *apinebulaErrorBody) string {
	if errObj == nil {
		return ""
	}
	return errObj.Message
}

func apinebulaTaskIDPresent(taskID, id string) bool {
	return strings.TrimSpace(taskID) != "" || strings.TrimSpace(id) != ""
}

func apinebulaChooseTaskID(taskID, id string) string {
	if v := strings.TrimSpace(taskID); v != "" {
		return v
	}
	return strings.TrimSpace(id)
}

// buildApinebulaSubmitBody marshals the normalized request onto ApiNebula's JSON
// submit body. Quality is omitted when absent; edit includes the ordered images
// array. model and prompt are always present (validated upstream).
func buildApinebulaSubmitBody(req ImageProviderRequest) ([]byte, error) {
	payload := map[string]any{
		"model":  req.Model,
		"prompt": req.Prompt,
	}
	if req.Quality != "" {
		payload["quality"] = req.Quality
	}
	if req.Operation == ImageOperationEdit {
		images := make([]map[string]string, 0, len(req.Images))
		for _, u := range req.Images {
			images = append(images, map[string]string{"image_url": u})
		}
		payload["images"] = images
	}
	body, err := common.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("apinebula: marshal submit body: %w", err)
	}
	return body, nil
}

// joinApinebulaURL concatenates a frozen endpoint with a path, tolerating a
// trailing slash on the endpoint. An empty endpoint is rejected by the caller's
// revision validation; this helper only normalizes the join.
func joinApinebulaURL(endpoint, path string) string {
	return strings.TrimRight(endpoint, "/") + path
}
