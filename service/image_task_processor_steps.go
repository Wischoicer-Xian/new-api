package service

import (
	"fmt"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"

	"gorm.io/gorm"
)

// imageTaskBackoffSeconds returns the exponential backoff in seconds for one
// retry count, capped at ImageTaskMaxBackoff. count starts at 1 (the first
// retry). The shift is bounded so a runaway count cannot overflow.
func imageTaskBackoffSeconds(count int) int64 {
	initial := int64(constant.ImageTaskInitialBackoff.Seconds())
	max := int64(constant.ImageTaskMaxBackoff.Seconds())
	if initial <= 0 {
		return max
	}
	if count <= 1 {
		return initial
	}
	shift := count - 1
	if shift > 24 {
		shift = 24
	}
	backoff := initial << shift
	if backoff <= 0 || backoff > max {
		return max
	}
	return backoff
}

// submissionReconcileSLASeconds is the wall-clock window a submission_unknown
// execution waits before routing to manual_review.
func submissionReconcileSLASeconds() int64 {
	return int64(constant.ImageTaskSubmissionReconcileSLA.Seconds())
}

// cancelDrainSLASeconds is the wall-clock window a cancel_requested execution
// drains the upstream before finalizing as cancelled.
func cancelDrainSLASeconds() int64 {
	return int64(constant.ImageTaskCancelDrainSLA.Seconds())
}

// processSubmit drives the queued → submit → (polling | submission_unknown |
// failed | manual_review) transition. ApiNebula accepts no upstream idempotency
// key, so a 429 (explicit rejection) is retried under the submit budget, while a
// network/5xx outcome routes to submission_unknown and is never re-submitted.
func (env *imageTaskProcessorEnv) processSubmit(summary *ImageTaskProcessorSummary) {
	task, err := model.GetImageTaskExecutionTask(env.exec)
	if err != nil {
		markManualReview(summary, env.adv, fmt.Sprintf("load backing task: %v", err))
		return
	}
	req, err := BuildImageProviderRequest(operationFromExecution(env.exec.Operation), task.Data)
	if err != nil {
		markManualReview(summary, env.adv, fmt.Sprintf("build provider request: %v", err))
		return
	}
	upstreamID, err := env.adapter.Submit(env.ctx, env.rev, env.credential, req)
	if err == nil {
		env.acceptSubmit(summary, upstreamID)
		return
	}
	perr := AsImageProviderError(err)
	if perr == nil {
		markManualReview(summary, env.adv, fmt.Sprintf("submit unclassified error: %v", err))
		return
	}
	switch perr.Kind {
	case ImageErrPreSubmitSafe, ImageErrTerminalProvider:
		env.finalizeFailed(summary, "provider rejected submit: "+perr.UpstreamMessage)
	case ImageErrManualReview:
		markManualReview(summary, env.adv, "submit manual_review: "+perr.UpstreamMessage)
	case ImageErrSubmissionUnknown:
		if perr.Status == 429 {
			env.retrySubmitRateLimited(summary)
		} else {
			env.routeSubmissionUnknown(summary, "submit outcome unknown: "+perr.UpstreamMessage)
		}
	default:
		markManualReview(summary, env.adv, fmt.Sprintf("submit unexpected kind %s: %s", perr.Kind, perr.UpstreamMessage))
	}
}

// acceptSubmit records the accepted upstream task id and moves to polling. The
// first poll is scheduled one initial-backoff out so the upstream has time to
// begin processing before the first (likely running) poll.
func (env *imageTaskProcessorEnv) acceptSubmit(summary *ImageTaskProcessorSummary, upstreamID string) {
	adv := env.adv
	adv.To = model.ImageTaskStatePolling
	won, err := model.AdvanceImageTaskExecutionCAS(adv, func(tx *gorm.DB) error {
		return tx.Model(&model.ImageTaskExecution{}).Where("id = ?", env.exec.ID).Updates(map[string]any{
			"client_submission_id": upstreamID,
			"submission_state":     "accepted",
			"next_run_at":          env.now + imageTaskBackoffSeconds(1),
		}).Error
	})
	if err != nil {
		logger.LogWarn(env.ctx, fmt.Sprintf("image task processor: accept submit advance execution %d: %v", env.exec.ID, err))
		return
	}
	if won {
		summary.StillInProgress++
	}
}

// retrySubmitRateLimited re-queues a 429-rejected submit under the submit budget.
// 429 is an explicit rejection so the request was not accepted upstream; a retry
// cannot duplicate. Budget exhaustion routes to manual_review.
func (env *imageTaskProcessorEnv) retrySubmitRateLimited(summary *ImageTaskProcessorSummary) {
	count := env.exec.SubmitErrorCount + 1
	if count > constant.ImageTaskSubmitErrorBudget {
		markManualReview(summary, env.adv, "submit 429 retry budget exhausted")
		return
	}
	won, err := model.AdvanceImageTaskExecutionCAS(env.adv, func(tx *gorm.DB) error {
		return tx.Model(&model.ImageTaskExecution{}).Where("id = ?", env.exec.ID).Updates(map[string]any{
			"submit_error_count": count,
			"submission_state":   "rate_limited",
			"next_run_at":        env.now + imageTaskBackoffSeconds(count),
		}).Error
	})
	if err != nil {
		logger.LogWarn(env.ctx, fmt.Sprintf("image task processor: 429 backoff execution %d: %v", env.exec.ID, err))
		return
	}
	if won {
		summary.StillInProgress++
	}
}

// routeSubmissionUnknown moves the execution into submission_unknown and arms the
// reconcile SLA. ApiNebula cannot be queried by a client id, so after the SLA
// the execution routes to manual_review; it is never re-submitted (duplicate
// risk, §7.5 effectively-once).
func (env *imageTaskProcessorEnv) routeSubmissionUnknown(summary *ImageTaskProcessorSummary, reason string) {
	adv := env.adv
	adv.To = model.ImageTaskStateSubmissionUnknown
	won, err := model.AdvanceImageTaskExecutionCAS(adv, func(tx *gorm.DB) error {
		return tx.Model(&model.ImageTaskExecution{}).Where("id = ?", env.exec.ID).Updates(map[string]any{
			"submission_state":     "unknown",
			"submit_error_count":   env.exec.SubmitErrorCount + 1,
			"next_run_at":          env.now + submissionReconcileSLASeconds(),
			"manual_review_reason": truncateReviewReason(reason),
		}).Error
	})
	if err != nil {
		logger.LogWarn(env.ctx, fmt.Sprintf("image task processor: route submission_unknown execution %d: %v", env.exec.ID, err))
		return
	}
	if won {
		summary.StillInProgress++
	}
}

// processReconcile handles a submission_unknown execution. With an upstream id
// on record it resumes polling; without one it routes to manual_review. ApiNebula
// accepts no client idempotency key, so submission_unknown is armed with
// next_run_at = entry + reconcile SLA: the execution is claimable again only
// after the SLA elapses, which is exactly when manual_review must fire. The
// UpdatedAt column cannot track entry time because every claim bumps it, so the
// SLA is encoded in next_run_at instead (one atomic CAS sets both state and the
// deadline, leaving no crash window).
func (env *imageTaskProcessorEnv) processReconcile(summary *ImageTaskProcessorSummary) {
	if env.exec.ClientSubmissionID != "" {
		env.advanceToPolling(summary, "reconcile resume")
		return
	}
	markManualReview(summary, env.adv, "submission unknown past reconcile SLA, no upstream id to query")
}

func (env *imageTaskProcessorEnv) advanceToPolling(summary *ImageTaskProcessorSummary, reason string) {
	adv := env.adv
	adv.To = model.ImageTaskStatePolling
	won, err := model.AdvanceImageTaskExecutionCAS(adv, func(tx *gorm.DB) error {
		return tx.Model(&model.ImageTaskExecution{}).Where("id = ?", env.exec.ID).Updates(map[string]any{
			"submission_state": reason,
			"next_run_at":      env.now + imageTaskBackoffSeconds(1),
		}).Error
	})
	if err != nil {
		logger.LogWarn(env.ctx, fmt.Sprintf("image task processor: advance to polling execution %d: %v", env.exec.ID, err))
		return
	}
	if won {
		summary.StillInProgress++
	}
}

// processPoll drives the polling loop: a completed result is persisted and the
// execution finalized; a running upstream backs off; a failed upstream refunds.
// A crash between result-store and finalize leaves SHA256 set, so a later claim
// re-finalizes without re-downloading.
func (env *imageTaskProcessorEnv) processPoll(summary *ImageTaskProcessorSummary) {
	if env.exec.Result.SHA256 != "" {
		env.finalizeCompleted(summary, env.exec.Result)
		return
	}
	if env.exec.Result.ContentURL != "" {
		env.resumeResultStore(summary, env.exec.Result.ContentURL)
		return
	}
	outcome, err := env.adapter.Poll(env.ctx, env.rev, env.credential, env.exec.ClientSubmissionID)
	if err != nil {
		env.handlePollError(summary, err)
		return
	}
	switch outcome.Status {
	case ImagePollCompleted:
		env.persistAndFinalize(summary, outcome.DownloadURL)
	case ImagePollFailed:
		env.finalizeFailed(summary, "provider task failed: "+outcome.Message)
	case ImagePollRunning:
		env.backoffPoll(summary)
	}
}

func (env *imageTaskProcessorEnv) persistAndFinalize(summary *ImageTaskProcessorSummary, downloadURL string) {
	locator, err := PersistImageTaskResult(env.ctx, env.exec, downloadURL)
	if err != nil {
		env.recordPendingResultAndBackoff(summary, downloadURL, err)
		return
	}
	env.finalizeCompleted(summary, locator)
}

func (env *imageTaskProcessorEnv) resumeResultStore(summary *ImageTaskProcessorSummary, downloadURL string) {
	locator, err := PersistImageTaskResult(env.ctx, env.exec, downloadURL)
	if err != nil {
		env.recordPendingResultAndBackoff(summary, downloadURL, err)
		return
	}
	env.finalizeCompleted(summary, locator)
}

// recordPendingResultAndBackoff caches the upstream download_url on the execution
// and backs off, so the next claim retries ONLY result persistence (never
// re-poll or re-submit). Budget exhaustion routes to manual_review.
func (env *imageTaskProcessorEnv) recordPendingResultAndBackoff(summary *ImageTaskProcessorSummary, downloadURL string, cause error) {
	count := env.exec.ResultErrorCount + 1
	if count > constant.ImageTaskResultErrorBudget {
		markManualReview(summary, env.adv, "result store budget exhausted: "+cause.Error())
		return
	}
	won, err := model.AdvanceImageTaskExecutionCAS(env.adv, func(tx *gorm.DB) error {
		return tx.Model(&model.ImageTaskExecution{}).Where("id = ?", env.exec.ID).Updates(map[string]any{
			"result":             model.ImageTaskResult{ContentURL: downloadURL},
			"result_error_count": count,
			"next_run_at":        env.now + imageTaskBackoffSeconds(count),
		}).Error
	})
	if err != nil {
		logger.LogWarn(env.ctx, fmt.Sprintf("image task processor: pending result backoff execution %d: %v", env.exec.ID, err))
		return
	}
	if won {
		summary.StillInProgress++
	}
}

func (env *imageTaskProcessorEnv) handlePollError(summary *ImageTaskProcessorSummary, err error) {
	perr := AsImageProviderError(err)
	if perr == nil {
		markManualReview(summary, env.adv, fmt.Sprintf("poll unclassified error: %v", err))
		return
	}
	switch perr.Kind {
	case ImageErrRetryablePoll:
		count := env.exec.PollErrorCount + 1
		if count > constant.ImageTaskPollErrorBudget {
			markManualReview(summary, env.adv, "poll retry budget exhausted: "+perr.UpstreamMessage)
			return
		}
		won, advErr := model.AdvanceImageTaskExecutionCAS(env.adv, func(tx *gorm.DB) error {
			return tx.Model(&model.ImageTaskExecution{}).Where("id = ?", env.exec.ID).Updates(map[string]any{
				"poll_error_count": count,
				"next_run_at":      env.now + imageTaskBackoffSeconds(count),
			}).Error
		})
		if advErr != nil {
			logger.LogWarn(env.ctx, fmt.Sprintf("image task processor: poll retry backoff execution %d: %v", env.exec.ID, advErr))
			return
		}
		if won {
			summary.StillInProgress++
		}
	case ImageErrManualReview:
		markManualReview(summary, env.adv, "poll manual_review: "+perr.UpstreamMessage)
	default:
		markManualReview(summary, env.adv, fmt.Sprintf("poll unexpected kind %s: %s", perr.Kind, perr.UpstreamMessage))
	}
}

func (env *imageTaskProcessorEnv) backoffPoll(summary *ImageTaskProcessorSummary) {
	count := env.exec.PollCount + 1
	won, err := model.AdvanceImageTaskExecutionCAS(env.adv, func(tx *gorm.DB) error {
		return tx.Model(&model.ImageTaskExecution{}).Where("id = ?", env.exec.ID).Updates(map[string]any{
			"poll_count":  count,
			"next_run_at": env.now + imageTaskBackoffSeconds(count),
		}).Error
	})
	if err != nil {
		logger.LogWarn(env.ctx, fmt.Sprintf("image task processor: poll running backoff execution %d: %v", env.exec.ID, err))
		return
	}
	if won {
		summary.StillInProgress++
	}
}

// processCancelDrain drains a cancel_requested execution to the upstream's
// terminal state (§7.5 cancel absorption). ApiNebula has no cancel endpoint, so
// the processor polls: a completed upstream finalizes completed (cost-correct,
// since ApiNebula would charge new-api), a failed upstream finalizes cancelled
// (refund), and a drain-SLA timeout finalizes cancelled to free user capacity.
func (env *imageTaskProcessorEnv) processCancelDrain(summary *ImageTaskProcessorSummary) {
	if env.exec.Result.SHA256 != "" {
		env.finalizeCompleted(summary, env.exec.Result)
		return
	}
	if env.exec.CancelRequestedAt != 0 && env.now-env.exec.CancelRequestedAt >= cancelDrainSLASeconds() {
		env.finalizeCancelled(summary, "cancel drain SLA exceeded, upstream not terminal")
		return
	}
	outcome, err := env.adapter.Poll(env.ctx, env.rev, env.credential, env.exec.ClientSubmissionID)
	if err != nil {
		// Transient poll errors during drain keep draining under the SLA; a
		// manual_review-class poll error (4xx) also keeps draining rather than
		// wedging the cancel, because the user's intent (cancel + refund) is
		// honored at the SLA deadline regardless.
		env.backoffCancelDrain(summary)
		return
	}
	switch outcome.Status {
	case ImagePollCompleted:
		env.persistAndFinalize(summary, outcome.DownloadURL)
	case ImagePollFailed:
		env.finalizeCancelled(summary, "upstream failed during cancel drain: "+outcome.Message)
	case ImagePollRunning:
		env.backoffCancelDrain(summary)
	}
}

func (env *imageTaskProcessorEnv) backoffCancelDrain(summary *ImageTaskProcessorSummary) {
	count := env.exec.PollCount + 1
	won, err := model.AdvanceImageTaskExecutionCAS(env.adv, func(tx *gorm.DB) error {
		return tx.Model(&model.ImageTaskExecution{}).Where("id = ?", env.exec.ID).Updates(map[string]any{
			"poll_count":  count,
			"next_run_at": env.now + imageTaskBackoffSeconds(count),
		}).Error
	})
	if err != nil {
		logger.LogWarn(env.ctx, fmt.Sprintf("image task processor: cancel drain backoff execution %d: %v", env.exec.ID, err))
		return
	}
	if won {
		summary.StillInProgress++
	}
}

// finalizeCompleted records the durable locator and finalizes the execution as
// completed. The locator write and the terminal CAS are two fenced steps so the
// billing aggregate (FinalizeImageTask) keeps its single-CAS billing invariant;
// a crash between them leaves SHA256 set and a later claim re-finalizes.
func (env *imageTaskProcessorEnv) finalizeCompleted(summary *ImageTaskProcessorSummary, locator model.ImageTaskResult) {
	won, err := model.AdvanceImageTaskExecutionCAS(env.adv, func(tx *gorm.DB) error {
		return tx.Model(&model.ImageTaskExecution{}).Where("id = ?", env.exec.ID).
			Update("result", locator).Error
	})
	if err != nil {
		logger.LogWarn(env.ctx, fmt.Sprintf("image task processor: record completed result execution %d: %v", env.exec.ID, err))
		return
	}
	if !won {
		return
	}
	env.finalizeTerminal(summary, model.ImageTaskStateCompleted, 0)
}

func (env *imageTaskProcessorEnv) finalizeFailed(summary *ImageTaskProcessorSummary, reason string) {
	reserve, err := model.GetImageTaskReserveQuotaAmount(env.exec.TaskDBID)
	if err != nil {
		markManualReview(summary, env.adv, "finalize failed: cannot read reserve quota: "+err.Error())
		return
	}
	logger.LogInfo(env.ctx, fmt.Sprintf("image task processor: execution %d finalized failed (refund=%d): %s", env.exec.ID, reserve, reason))
	env.finalizeTerminal(summary, model.ImageTaskStateFailed, -reserve)
}

func (env *imageTaskProcessorEnv) finalizeCancelled(summary *ImageTaskProcessorSummary, reason string) {
	reserve, err := model.GetImageTaskReserveQuotaAmount(env.exec.TaskDBID)
	if err != nil {
		markManualReview(summary, env.adv, "finalize cancelled: cannot read reserve quota: "+err.Error())
		return
	}
	logger.LogInfo(env.ctx, fmt.Sprintf("image task processor: execution %d finalized cancelled (refund=%d): %s", env.exec.ID, reserve, reason))
	env.finalizeTerminal(summary, model.ImageTaskStateCancelled, -reserve)
}

// finalizeTerminal wraps FinalizeImageTask, mapping the won/CAS outcome onto the
// summary. FromState is the execution's current (pre-terminal) state; the fence
// inside FinalizeImageTask rejects a stale lease or a raced transition.
func (env *imageTaskProcessorEnv) finalizeTerminal(summary *ImageTaskProcessorSummary, to model.ImageTaskExecutionState, settle int) {
	cmd := model.ImageTaskFinalizeCommand{
		ExecutionID:     env.exec.ID,
		FromState:       env.exec.State,
		ToState:         to,
		LeaseOwner:      env.adv.LeaseOwner,
		LeaseGeneration: env.adv.ExpectedGeneration,
		Now:             env.now,
		SettleAmount:    settle,
	}
	won, err := model.FinalizeImageTask(env.ctx, cmd)
	if err != nil {
		logger.LogWarn(env.ctx, fmt.Sprintf("image task processor: finalize execution %d → %s: %v", env.exec.ID, to, err))
		return
	}
	if !won {
		return
	}
	switch to {
	case model.ImageTaskStateCompleted:
		summary.Completed++
	case model.ImageTaskStateFailed:
		summary.Failed++
	case model.ImageTaskStateCancelled:
		summary.Cancelled++
	}
}

// truncateReviewReason caps a manual_review reason at the column width so a long
// upstream message never overflows manual_review_reason varchar(255).
func truncateReviewReason(reason string) string {
	const capLen = 250
	if len(reason) <= capLen {
		return reason
	}
	return reason[:capLen]
}
