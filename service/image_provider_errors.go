package service

import (
	"errors"
	"fmt"
	"io"
	"net/http"
)

// ImageProviderStage names the provider-execution step an error originated from.
// The processor keeps an independent error budget per stage (§7.5).
type ImageProviderStage string

const (
	ImageProviderStageSubmit      ImageProviderStage = "submit"
	ImageProviderStagePoll        ImageProviderStage = "poll"
	ImageProviderStageResultStore ImageProviderStage = "result_store"
)

// ImageProviderErrorKind is the §7.5 error classification. It is the sole input
// the processor state machine uses to decide the next state, the backoff, and
// whether to route to manual_review. Channel switching is forbidden (§7.5), so
// pre_submit_safe never means "try another channel" here — it finalizes the
// execution as failed because the request is invalid for the frozen channel.
type ImageProviderErrorKind string

const (
	// ImageErrAccepted signals a successful submit: the provider returned a task
	// id. It is carried as a non-error outcome (Submit returns nil error), but
	// the kind exists so tests and logs share one vocabulary.
	ImageErrAccepted ImageProviderErrorKind = "accepted"
	// ImageErrPreSubmitSafe is a 4xx submit response (except 409/429): the
	// provider rejected the request as invalid. §7.5 forbids channel switching,
	// so the processor finalizes the execution as failed (refund).
	ImageErrPreSubmitSafe ImageProviderErrorKind = "pre_submit_safe"
	// ImageErrSubmissionUnknown means the submit outcome is unknowable (network
	// error, timeout, 5xx, or a 2xx body without a task id). ApiNebula accepts
	// no client idempotency key upstream, so the processor must NOT re-submit
	// (duplicate risk); it transitions to submission_unknown and, after the
	// reconcile SLA, to manual_review.
	ImageErrSubmissionUnknown ImageProviderErrorKind = "submission_unknown"
	// ImageErrRetryablePoll is a transient poll failure (5xx, 429, network). The
	// processor backs off and re-polls the same task; the budget guards against
	// an unbounded retry loop.
	ImageErrRetryablePoll ImageProviderErrorKind = "retryable_poll"
	// ImageErrTerminalProvider means the provider reported the task itself as
	// failed. The processor finalizes the execution as failed (refund).
	ImageErrTerminalProvider ImageProviderErrorKind = "terminal_provider"
	// ImageErrResultStore means downloading, validating, or persisting the
	// completed image failed. Only result persistence retries (never re-submit
	// or re-poll); the budget guards the retry loop.
	ImageErrResultStore ImageProviderErrorKind = "result_store_error"
	// ImageErrManualReview routes the execution to manual_review directly: a
	// self-idempotency 409 (implementation-defect signal, §7.5), a poll 4xx that
	// will not self-heal, or a budget exhaustion. An operator binds the upstream
	// id and rules the billing by audit.
	ImageErrManualReview ImageProviderErrorKind = "manual_review"
)

// ImageProviderError is the typed provider error. Kind drives the processor;
// Status/Stage/TaskID/UpstreamMessage carry the audit detail the processor
// writes into the execution row's submission_state / manual_review_reason.
type ImageProviderError struct {
	Kind           ImageProviderErrorKind
	Stage          ImageProviderStage
	Status         int    // HTTP status; 0 when the failure is not HTTP-shaped.
	TaskID         string // upstream task id, when the outcome still yielded one.
	UpstreamMessage string // provider error message or network error text.
	Err            error  // wrapped cause; nil when the error is purely classified.
}

func (e *ImageProviderError) Error() string {
	if e == nil {
		return "image provider error"
	}
	if e.Err != nil {
		return fmt.Sprintf("image provider %s %s (status=%d): %s: %v", e.Stage, e.Kind, e.Status, e.UpstreamMessage, e.Err)
	}
	return fmt.Sprintf("image provider %s %s (status=%d): %s", e.Stage, e.Kind, e.Status, e.UpstreamMessage)
}

func (e *ImageProviderError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// AsImageProviderError unwraps a *ImageProviderError from err, returning nil when
// err is not one. The processor uses it to branch on Kind.
func AsImageProviderError(err error) *ImageProviderError {
	var target *ImageProviderError
	if errors.As(err, &target) {
		return target
	}
	return nil
}

// classifySubmitHTTP maps a submit HTTP response onto a provider error kind.
// The body has already been read by the caller; status is the response status.
// A 2xx without a parseable task id is submission_unknown: the provider spoke
// the protocol but did not yield a durable handle, so the outcome is unknowable
// and the processor must not guess. 409 is the self-idempotency defect signal
// (§7.5) and routes straight to manual_review.
func classifySubmitHTTP(status int, hasTaskID bool, upstreamMsg string) *ImageProviderError {
	switch {
	case status >= 200 && status < 300:
		if hasTaskID {
			return nil // accepted
		}
		return &ImageProviderError{
			Kind: ImageErrSubmissionUnknown, Stage: ImageProviderStageSubmit,
			Status: status, UpstreamMessage: "2xx submit response missing task id",
		}
	case status == http.StatusConflict:
		return &ImageProviderError{
			Kind: ImageErrManualReview, Stage: ImageProviderStageSubmit,
			Status: status, UpstreamMessage: upstreamMsg,
		}
	case status == http.StatusTooManyRequests:
		// 429 is an explicit rejection, not an unknown outcome: retrying the
		// same submit is safe. Classified as submission_unknown so the processor
		// re-submits under the submit budget with backoff (no channel switch).
		return &ImageProviderError{
			Kind: ImageErrSubmissionUnknown, Stage: ImageProviderStageSubmit,
			Status: status, UpstreamMessage: upstreamMsg,
		}
	case status >= 400 && status < 500:
		return &ImageProviderError{
			Kind: ImageErrPreSubmitSafe, Stage: ImageProviderStageSubmit,
			Status: status, UpstreamMessage: upstreamMsg,
		}
	default: // 5xx
		return &ImageProviderError{
			Kind: ImageErrSubmissionUnknown, Stage: ImageProviderStageSubmit,
			Status: status, UpstreamMessage: upstreamMsg,
		}
	}
}

// classifyPollHTTP maps a poll HTTP response onto a provider error kind. Poll
// failures split into retryable (5xx/429/network — backoff and re-poll) and
// manual-review (4xx — a config or protocol issue that will not self-heal).
// 404 specifically means the upstream no longer knows the task id, which only
// an operator can reconcile.
func classifyPollHTTP(status int, upstreamMsg string) *ImageProviderError {
	switch {
	case status >= 200 && status < 300:
		return nil // the caller interprets the body for running/completed/failed
	case status == http.StatusTooManyRequests || status >= 500:
		return &ImageProviderError{
			Kind: ImageErrRetryablePoll, Stage: ImageProviderStagePoll,
			Status: status, UpstreamMessage: upstreamMsg,
		}
	default: // 4xx (including 404)
		return &ImageProviderError{
			Kind: ImageErrManualReview, Stage: ImageProviderStagePoll,
			Status: status, UpstreamMessage: upstreamMsg,
		}
	}
}

// networkSubmitError classifies a submit that never received an HTTP response
// (connection refused, DNS, timeout, EOF). The outcome is unknowable: the
// request may or may not have been accepted upstream.
func networkSubmitError(stage ImageProviderStage, err error) *ImageProviderError {
	return &ImageProviderError{
		Kind: ImageErrSubmissionUnknown, Stage: stage,
		UpstreamMessage: "submit network/timeout error",
		Err:             err,
	}
}

// networkPollError classifies a poll that never received an HTTP response. It is
// retryable: a transient network blip does not change the upstream task state.
func networkPollError(err error) *ImageProviderError {
	return &ImageProviderError{
		Kind: ImageErrRetryablePoll, Stage: ImageProviderStagePoll,
		UpstreamMessage: "poll network/timeout error",
		Err:             err,
	}
}

// resultStoreError classifies a result download/validate/persist failure. Only
// result persistence retries; the processor never re-submits or re-polls on it.
func resultStoreError(err error) *ImageProviderError {
	return &ImageProviderError{
		Kind: ImageErrResultStore, Stage: ImageProviderStageResultStore,
		UpstreamMessage: "result store error",
		Err:             err,
	}
}

// readBodyClose drains and closes the response body so the HTTP transport can
// reuse the connection, returning the bytes for classification. A read failure
// is reported as the supplied network-error constructor so a truncated body is
// treated consistently with a transport failure for the originating stage.
func readBodyClose(resp *http.Response, networkErr func(error) *ImageProviderError) ([]byte, *ImageProviderError) {
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, networkErr(err)
	}
	return body, nil
}
