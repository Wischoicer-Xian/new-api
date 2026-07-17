package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// apinebulaTestServer returns an httptest server whose handler dispatches on the
// request path to return canned ApiNebula fixtures. The handler records the last
// submit body so the test can assert the wire shape. Close is the caller's job.
type apinebulaTestServer struct {
	server    *httptest.Server
	lastBody  string
	lastAuth  string
	pollCount int
	// pollStatuses, when non-empty, cycles through canned poll responses so a
	// single test can observe a running-then-completed progression.
	pollStatuses []string
}

func newApinebulaTestServer(t *testing.T) *apinebulaTestServer {
	t.Helper()
	ts := &apinebulaTestServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/image-tasks/generations", func(w http.ResponseWriter, r *http.Request) {
		ts.capture(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"task_fixturesubmit001","task_id":"task_fixturesubmit001","object":"image.task","model":"gpt-image-2","status":"queued","created_at":1700000000}`))
	})
	mux.HandleFunc("/v1/image-tasks/edits", func(w http.ResponseWriter, r *http.Request) {
		ts.capture(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"task_editfix001","task_id":"task_editfix001","object":"image.task","model":"adobe-gpt-image-2","status":"queued","created_at":1700000000}`))
	})
	mux.HandleFunc("/v1/image-tasks/", func(w http.ResponseWriter, r *http.Request) {
		ts.pollCount++
		ts.lastAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		status := "in_progress"
		if len(ts.pollStatuses) > 0 {
			idx := ts.pollCount - 1
			if idx >= len(ts.pollStatuses) {
				idx = len(ts.pollStatuses) - 1
			}
			status = ts.pollStatuses[idx]
		}
		switch status {
		case "completed", "succeeded":
			_, _ = w.Write([]byte(`{"id":"task_fixturesubmit001","object":"image","model":"gpt-image-2","status":"completed","created_at":1700000000,"completed_at":1700000060,"detail":{"data":[{"download_url":"https://fixtures.example.com/img.png","response_format":"b64_json"}]}}`))
		case "failed", "cancelled":
			_, _ = w.Write([]byte(`{"id":"task_fixturesubmit001","object":"image","model":"gpt-image-2","status":"failed","created_at":1700000000,"completed_at":1700000060,"error":{"message":"content policy violation"}}`))
		default:
			_, _ = w.Write([]byte(`{"id":"task_fixturesubmit001","object":"image.task","model":"gpt-image-2","status":"` + status + `","created_at":1700000000}`))
		}
	})
	ts.server = httptest.NewServer(mux)
	t.Cleanup(ts.server.Close)
	return ts
}

func (ts *apinebulaTestServer) capture(r *http.Request) {
	ts.lastAuth = r.Header.Get("Authorization")
	if r.Body != nil {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		ts.lastBody = string(buf[:n])
	}
}

// useApinebulaTestClient points the adapter's HTTP seam at the httptest server.
func useApinebulaTestClient(t *testing.T, ts *apinebulaTestServer) {
	t.Helper()
	original := apinebulaHTTPClient
	apinebulaHTTPClient = func(string) (*http.Client, error) { return ts.server.Client(), nil }
	t.Cleanup(func() { apinebulaHTTPClient = original })
}

func apinebulaTestRev(endpoint string) *model.ChannelRevision {
	return &model.ChannelRevision{ID: 1, ChannelID: 42, Endpoint: endpoint, CredentialRef: "channel:42", AdapterVersion: constant.ImageAdapterVersionApiNebula}
}

func TestApinebulaSubmitGenerationAccepted(t *testing.T) {
	ts := newApinebulaTestServer(t)
	useApinebulaTestClient(t, ts)
	adapter := &apinebulaAdapter{}

	id, err := adapter.Submit(context.Background(), apinebulaTestRev(ts.server.URL), "sk-test-key",
		ImageProviderRequest{Operation: ImageOperationGeneration, Model: "gpt-image-2", Prompt: "a red cube"})
	require.NoError(t, err)
	assert.Equal(t, "task_fixturesubmit001", id)
	assert.Equal(t, "Bearer sk-test-key", ts.lastAuth)
	assert.Contains(t, ts.lastBody, `"model":"gpt-image-2"`)
	assert.Contains(t, ts.lastBody, `"prompt":"a red cube"`)
	assert.NotContains(t, ts.lastBody, "quality", "quality omitted when absent")
	assert.NotContains(t, ts.lastBody, "images", "images omitted for generation")
}

func TestApinebulaSubmitEditIncludesImages(t *testing.T) {
	ts := newApinebulaTestServer(t)
	useApinebulaTestClient(t, ts)
	adapter := &apinebulaAdapter{}

	id, err := adapter.Submit(context.Background(), apinebulaTestRev(ts.server.URL), "sk-test-key",
		ImageProviderRequest{Operation: ImageOperationEdit, Model: "adobe-gpt-image-2", Prompt: "extend", Quality: "high", Images: []string{"https://fixtures.example.com/in.png"}})
	require.NoError(t, err)
	assert.Equal(t, "task_editfix001", id)
	assert.Contains(t, ts.lastBody, `"quality":"high"`)
	assert.Contains(t, ts.lastBody, `"image_url":"https://fixtures.example.com/in.png"`)
}

func TestApinebulaSubmitClassifications(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantKind ImageProviderErrorKind
	}{
		{name: "400 pre_submit_safe", status: 400, body: `{"error":{"message":"bad model"}}`, wantKind: ImageErrPreSubmitSafe},
		{name: "409 manual_review", status: 409, body: `{"error":{"message":"conflict"}}`, wantKind: ImageErrManualReview},
		{name: "429 submission_unknown retryable", status: 429, body: `{"error":{"message":"slow down"}}`, wantKind: ImageErrSubmissionUnknown},
		{name: "500 submission_unknown", status: 500, body: `{"error":{"message":"oops"}}`, wantKind: ImageErrSubmissionUnknown},
		{name: "2xx missing task id unknown", status: 200, body: `{"status":"queued"}`, wantKind: ImageErrSubmissionUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srv.Close)
			useApinebulaTestClient(t, &apinebulaTestServer{server: srv})
			adapter := &apinebulaAdapter{}
			_, err := adapter.Submit(context.Background(), apinebulaTestRev(srv.URL), "k",
				ImageProviderRequest{Operation: ImageOperationGeneration, Model: "m", Prompt: "p"})
			perr := AsImageProviderError(err)
			require.NotNil(t, err, "expected classified error")
			require.NotNil(t, perr)
			assert.Equal(t, tt.wantKind, perr.Kind)
		})
	}
}

func TestApinebulaPollOutcomes(t *testing.T) {
	tests := []struct {
		name        string
		statuses    []string
		wantStatus  ImageAdapterPollStatus
		wantURL     string
		wantMessage string
	}{
		{name: "queued running", statuses: []string{"queued"}, wantStatus: ImagePollRunning},
		{name: "in_progress running", statuses: []string{"in_progress"}, wantStatus: ImagePollRunning},
		{name: "waiting running", statuses: []string{"waiting"}, wantStatus: ImagePollRunning},
		{name: "completed", statuses: []string{"completed"}, wantStatus: ImagePollCompleted, wantURL: "https://fixtures.example.com/img.png"},
		{name: "succeeded completed", statuses: []string{"succeeded"}, wantStatus: ImagePollCompleted, wantURL: "https://fixtures.example.com/img.png"},
		{name: "failed", statuses: []string{"failed"}, wantStatus: ImagePollFailed, wantMessage: "content policy violation"},
		{name: "cancelled failed", statuses: []string{"cancelled"}, wantStatus: ImagePollFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := newApinebulaTestServer(t)
			ts.pollStatuses = tt.statuses
			useApinebulaTestClient(t, ts)
			adapter := &apinebulaAdapter{}
			outcome, err := adapter.Poll(context.Background(), apinebulaTestRev(ts.server.URL), "k", "task_fixturesubmit001")
			require.NoError(t, err)
			assert.Equal(t, tt.wantStatus, outcome.Status)
			if tt.wantURL != "" {
				assert.Equal(t, tt.wantURL, outcome.DownloadURL)
			}
			if tt.wantMessage != "" {
				assert.Equal(t, tt.wantMessage, outcome.Message)
			}
		})
	}
}

func TestApinebulaPollErrorClassifications(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		wantKind ImageProviderErrorKind
	}{
		{name: "429 retryable_poll", status: 429, wantKind: ImageErrRetryablePoll},
		{name: "500 retryable_poll", status: 500, wantKind: ImageErrRetryablePoll},
		{name: "404 manual_review", status: 404, wantKind: ImageErrManualReview},
		{name: "401 manual_review", status: 401, wantKind: ImageErrManualReview},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"error":{"message":"x"}}`))
			}))
			t.Cleanup(srv.Close)
			useApinebulaTestClient(t, &apinebulaTestServer{server: srv})
			adapter := &apinebulaAdapter{}
			_, err := adapter.Poll(context.Background(), apinebulaTestRev(srv.URL), "k", "task_x")
			perr := AsImageProviderError(err)
			require.NotNil(t, perr)
			assert.Equal(t, tt.wantKind, perr.Kind)
		})
	}
}

func TestApinebulaCancelUnsupported(t *testing.T) {
	adapter := &apinebulaAdapter{}
	err := adapter.Cancel(context.Background(), apinebulaTestRev("https://x"), "k", "task_x")
	assert.ErrorIs(t, err, ErrCancelUnsupported)
}

func TestApinebulaPollEmptyTaskIDManualReview(t *testing.T) {
	adapter := &apinebulaAdapter{}
	_, err := adapter.Poll(context.Background(), apinebulaTestRev("https://x"), "k", "")
	perr := AsImageProviderError(err)
	require.NotNil(t, perr)
	assert.Equal(t, ImageErrManualReview, perr.Kind)
}

func TestApinebulaVersionLabelMatchesConstant(t *testing.T) {
	// The adapter registers itself under the constant version label; a frozen
	// revision created under the constant must resolve the adapter. The
	// capability registry entry must carry the same string.
	caps, ok := ImageAdapterCapabilities(constant.APITypeApiNebula)
	require.True(t, ok)
	version, ok := ImageAdapterVersion(constant.APITypeApiNebula)
	require.True(t, ok)
	require.Equal(t, constant.ImageAdapterVersionApiNebula, version)
	// ApiNebula declares async_task for both operations.
	support := caps.ImageTaskExecutionSupport(ImageOperationGeneration)
	found := false
	for _, m := range support {
		if m == ImageExecutionAsyncTask {
			found = true
		}
	}
	assert.True(t, found, "generation must declare async_task")
}

func TestResolveImageTaskAdapterRegistered(t *testing.T) {
	adapter, ok := ResolveImageTaskAdapter(constant.ImageAdapterVersionApiNebula)
	require.True(t, ok)
	require.NotNil(t, adapter)

	_, ok = ResolveImageTaskAdapter("not-a-real-adapter-version")
	assert.False(t, ok)
}

func TestNormalizeApinebulaStatusTable(t *testing.T) {
	tests := []struct {
		raw  string
		want apinebulaStatusKind
	}{
		{"queued", apinebulaStatusRunning},
		{"QUEUED", apinebulaStatusRunning},
		{" waiting ", apinebulaStatusRunning},
		{"running", apinebulaStatusRunning},
		{"in_progress", apinebulaStatusRunning},
		{"succeeded", apinebulaStatusCompleted},
		{"completed", apinebulaStatusCompleted},
		{"failed", apinebulaStatusFailed},
		{"cancelled", apinebulaStatusFailed},
		{"canceled", apinebulaStatusFailed},
		{"", apinebulaStatusUnknown},
		{"weird-state", apinebulaStatusUnknown},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, normalizeApinebulaStatus(tt.raw), tt.raw)
	}
}

func TestBuildApinebulaSubmitBodyShape(t *testing.T) {
	body, err := buildApinebulaSubmitBody(ImageProviderRequest{Operation: ImageOperationGeneration, Model: "m", Prompt: "p"})
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(body, &decoded))
	assert.Equal(t, "m", decoded["model"])
	assert.Equal(t, "p", decoded["prompt"])
	_, hasQuality := decoded["quality"]
	assert.False(t, hasQuality)
	_, hasImages := decoded["images"]
	assert.False(t, hasImages)
}

func TestJoinApinebulaURLStripsTrailingSlash(t *testing.T) {
	assert.Equal(t, "https://x/v1/y", joinApinebulaURL("https://x/", "/v1/y"))
	assert.Equal(t, "https://x/v1/y", joinApinebulaURL("https://x", "/v1/y"))
}

func TestBuildImageProviderRequestGenerationAndEdit(t *testing.T) {
	gen := `{"model":"gpt-image-2","prompt":"p"}`
	req, err := BuildImageProviderRequest(ImageOperationGeneration, []byte(gen))
	require.NoError(t, err)
	assert.Equal(t, ImageOperationGeneration, req.Operation)
	assert.Empty(t, req.Images)

	quality := "high"
	edit := `{"model":"gpt-image-2","prompt":"p","quality":"high","images":[{"image_url":"https://a.example/1.png"},{"image_url":"https://a.example/2.png"}]}`
	req, err = BuildImageProviderRequest(ImageOperationEdit, []byte(edit))
	require.NoError(t, err)
	assert.Equal(t, ImageOperationEdit, req.Operation)
	assert.Equal(t, quality, req.Quality)
	require.Len(t, req.Images, 2)
	assert.Equal(t, "https://a.example/1.png", req.Images[0])

	// immutability: mutating the returned slice does not affect a re-build.
	req.Images[0] = "mutated"
	req2, err := BuildImageProviderRequest(ImageOperationEdit, []byte(edit))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(req2.Images[0], "https://"), "rebuild must not see the mutation")
}

func TestBuildImageProviderRequestRejectsEmpty(t *testing.T) {
	_, err := BuildImageProviderRequest(ImageOperationGeneration, nil)
	assert.Error(t, err)
}
