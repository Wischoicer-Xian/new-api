package service

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifySubmitHTTP(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		hasTaskID bool
		upstream  string
		wantKind  ImageProviderErrorKind
		wantNil   bool
	}{
		{name: "2xx with task id accepted", status: 200, hasTaskID: true, wantNil: true},
		{name: "201 accepted", status: 201, hasTaskID: true, wantNil: true},
		{name: "2xx missing task id unknown", status: 200, hasTaskID: false, wantKind: ImageErrSubmissionUnknown},
		{name: "400 pre_submit_safe", status: 400, hasTaskID: false, wantKind: ImageErrPreSubmitSafe},
		{name: "404 pre_submit_safe", status: 404, hasTaskID: false, wantKind: ImageErrPreSubmitSafe},
		{name: "409 manual_review defect signal", status: 409, wantKind: ImageErrManualReview},
		{name: "429 submission_unknown retryable", status: 429, wantKind: ImageErrSubmissionUnknown},
		{name: "500 submission_unknown", status: 500, wantKind: ImageErrSubmissionUnknown},
		{name: "503 submission_unknown", status: 503, wantKind: ImageErrSubmissionUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifySubmitHTTP(tt.status, tt.hasTaskID, tt.upstream)
			if tt.wantNil {
				require.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, tt.wantKind, got.Kind)
			assert.Equal(t, ImageProviderStageSubmit, got.Stage)
			assert.Equal(t, tt.status, got.Status)
		})
	}
}

func TestClassifyPollHTTP(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		wantKind ImageProviderErrorKind
		wantNil  bool
	}{
		{name: "200 nil", status: 200, wantNil: true},
		{name: "299 nil", status: 299, wantNil: true},
		{name: "429 retryable_poll", status: 429, wantKind: ImageErrRetryablePoll},
		{name: "500 retryable_poll", status: 500, wantKind: ImageErrRetryablePoll},
		{name: "502 retryable_poll", status: 502, wantKind: ImageErrRetryablePoll},
		{name: "400 manual_review", status: 400, wantKind: ImageErrManualReview},
		{name: "401 manual_review auth", status: 401, wantKind: ImageErrManualReview},
		{name: "404 manual_review lost task", status: 404, wantKind: ImageErrManualReview},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyPollHTTP(tt.status, "msg")
			if tt.wantNil {
				require.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, tt.wantKind, got.Kind)
			assert.Equal(t, ImageProviderStagePoll, got.Stage)
			assert.Equal(t, tt.status, got.Status)
		})
	}
}

func TestNetworkAndResultStoreErrorConstructors(t *testing.T) {
	cause := errors.New("boom")

	submit := networkSubmitError(ImageProviderStageSubmit, cause)
	require.NotNil(t, submit)
	assert.Equal(t, ImageErrSubmissionUnknown, submit.Kind)
	assert.Equal(t, ImageProviderStageSubmit, submit.Stage)
	assert.ErrorIs(t, submit, cause)

	poll := networkPollError(cause)
	require.NotNil(t, poll)
	assert.Equal(t, ImageErrRetryablePoll, poll.Kind)
	assert.ErrorIs(t, poll, cause)

	store := resultStoreError(cause)
	require.NotNil(t, store)
	assert.Equal(t, ImageErrResultStore, store.Kind)
	assert.ErrorIs(t, store, cause)
}

func TestAsImageProviderError(t *testing.T) {
	original := &ImageProviderError{Kind: ImageErrTerminalProvider, Stage: ImageProviderStagePoll, Status: http.StatusBadGateway}
	wrapped := wrapImageProviderErrorForTest(original)

	extracted := AsImageProviderError(wrapped)
	require.NotNil(t, extracted)
	assert.Equal(t, ImageErrTerminalProvider, extracted.Kind)

	assert.Nil(t, AsImageProviderError(errors.New("not a provider error")))
}

// wrapImageProviderErrorForTest wraps a provider error one level so the test
// exercises the errors.As unwrap path rather than a trivial identity match.
func wrapImageProviderErrorForTest(err error) error {
	return &testWrapper{err: err}
}

type testWrapper struct{ err error }

func (t *testWrapper) Error() string { return "wrapped: " + t.err.Error() }
func (t *testWrapper) Unwrap() error { return t.err }

func TestImageProviderErrorString(t *testing.T) {
	withCause := &ImageProviderError{Kind: ImageErrRetryablePoll, Stage: ImageProviderStagePoll, Status: 500, UpstreamMessage: "boom", Err: errors.New("timeout")}
	assert.Contains(t, withCause.Error(), "retryable_poll")
	assert.Contains(t, withCause.Error(), "timeout")

	withoutCause := &ImageProviderError{Kind: ImageErrPreSubmitSafe, Stage: ImageProviderStageSubmit, Status: 400, UpstreamMessage: "bad model"}
	assert.Contains(t, withoutCause.Error(), "pre_submit_safe")
	assert.Contains(t, withoutCause.Error(), "bad model")
}
