package service

import (
	"context"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
)

// ImageTaskProcessorSummary records one bounded due-work pass for the
// image_task_processor system task row, so an operator can observe throughput
// and stuck-task routing from the admin task view.
type ImageTaskProcessorSummary struct {
	DueCandidates   int `json:"due_candidates"`
	Claimed         int `json:"claimed"`
	FairnessSkipped int `json:"fairness_skipped"`
	ClaimLost       int `json:"claim_lost"`
	Completed       int `json:"completed"`
	Failed          int `json:"failed"`
	Cancelled       int `json:"cancelled"`
	ManualReview    int `json:"manual_review"`
	StillInProgress int `json:"still_in_progress"`
}

// imageTaskProcessorClock is the clock seam. Production returns common.GetTimestamp;
// tests override it for deterministic backoff/SLA assertions.
var imageTaskProcessorClock = common.GetTimestamp

// imageTaskProcessorStepTimeout is a test seam for the per-network-step
// deadline. Production keeps it below the execution lease.
var imageTaskProcessorStepTimeout = constant.ImageTaskProcessorStepTimeout

// imageTaskProcessorLeaseOwner returns the stable worker identity that holds
// execution leases during one pass. It is node-scoped so two masters do not
// share a lease owner; the per-row lease_generation is the fencing token.
func imageTaskProcessorLeaseOwner() string {
	return fmt.Sprintf("image-proc-%s", common.NodeName)
}

// RunImageTaskProcessorOnce performs one bounded due-work pass over due image
// task executions (§7.5). It is driven by the image_task_processor scheduled
// system task; the 15 s scheduler is the fallback cadence, not a sleep loop.
// The pass is fail-safe: a transient error on one execution is logged and does
// not abort the pass, and every state change is a fenced CAS so a stale worker
// can never clobber a live one.
//
// The gate (constant.ImageTaskProcessorEnabled) is checked by the handler, not
// here, so unit tests can drive the pass directly.
func RunImageTaskProcessorOnce(ctx context.Context) ImageTaskProcessorSummary {
	summary := ImageTaskProcessorSummary{}
	listNow := imageTaskProcessorClock()
	candidates, err := model.ListDueImageTaskExecutions(listNow, constant.ImageTaskProcessorDuePageSize)
	if err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("image task processor: list due candidates: %v", err))
		return summary
	}
	summary.DueCandidates = len(candidates)
	if len(candidates) == 0 {
		return summary
	}

	owner := imageTaskProcessorLeaseOwner()
	perUserLeased := make(map[int]int)

	for i := range candidates {
		candidate := candidates[i]
		if perUserLeased[candidate.OwnerUserID] >= constant.MaxImageTasksInFlightPerUser {
			summary.FairnessSkipped++
			continue
		}
		now := imageTaskProcessorClock()
		leaseUntil := now + int64(constant.ImageTaskProcessorClaimLease.Seconds())
		won, exec, claimErr := model.TryClaimImageTaskExecution(model.ImageTaskLeaseClaim{
			ExecutionID: candidate.ID, Owner: owner, Now: now, LeaseUntil: leaseUntil,
		})
		if claimErr != nil {
			logger.LogWarn(ctx, fmt.Sprintf("image task processor: claim execution %d: %v", candidate.ID, claimErr))
			continue
		}
		if !won {
			summary.ClaimLost++
			continue
		}
		summary.Claimed++
		perUserLeased[candidate.OwnerUserID]++

		delta := processImageTaskExecution(ctx, exec, now, owner)
		mergeProcessorSummary(&summary, delta)
	}
	return summary
}

// processImageTaskExecution loads the frozen revision, resolves the adapter and
// credential, and dispatches one execution by its state. It never panics on a
// missing revision/adapter/credential: those route to manual_review so an
// operator can reconcile rather than the execution silently looping.
func processImageTaskExecution(ctx context.Context, exec *model.ImageTaskExecution, now int64, leaseOwner string) ImageTaskProcessorSummary {
	summary := ImageTaskProcessorSummary{}
	adv := baseAdvance(exec, now, leaseOwner)

	// The processor always loads the FROZEN revision by id (§7.2), never the
	// latest revision for the channel: a later channel edit must not change an
	// in-flight task's endpoint or credential.
	rev, err := model.GetChannelRevisionByID(exec.ChannelRevisionID)
	if err != nil {
		markManualReview(&summary, adv, fmt.Sprintf("frozen channel revision %d not found: %v", exec.ChannelRevisionID, err))
		return summary
	}
	adapter, ok := ResolveImageTaskAdapter(exec.AdapterVersion)
	if !ok {
		markManualReview(&summary, adv, fmt.Sprintf("no adapter registered for version %q", exec.AdapterVersion))
		return summary
	}
	credential, err := ResolveChannelCredential(rev.CredentialRef)
	if err != nil {
		markManualReview(&summary, adv, fmt.Sprintf("credential unresolvable: %v", err))
		return summary
	}

	env := imageTaskProcessorEnv{
		ctx:        ctx,
		exec:       exec,
		rev:        rev,
		adapter:    adapter,
		credential: credential,
		now:        now,
		adv:        adv,
	}

	switch exec.State {
	case model.ImageTaskStateQueued:
		env.processSubmit(&summary)
	case model.ImageTaskStateSubmitting:
		// ApiNebula never persists submitting; a row here is a recovered
		// in-flight submit with no durable upstream id → submission_unknown.
		env.routeSubmissionUnknown(&summary, "recovered submitting state with no upstream id")
	case model.ImageTaskStateSubmissionUnknown:
		env.processReconcile(&summary)
	case model.ImageTaskStatePolling:
		env.processPoll(&summary)
	case model.ImageTaskStateCancelRequested:
		env.processCancelDrain(&summary)
	default:
		// Terminal or manual_review are not claimable; reaching here is a bug.
		logger.LogWarn(ctx, fmt.Sprintf("image task processor: execution %d in unexpected claimable state %q", exec.ID, exec.State))
	}
	return summary
}

// imageTaskProcessorEnv carries the per-execution resolved context the step
// functions need, so each step reads as a flat block rather than threading
// five arguments.
type imageTaskProcessorEnv struct {
	ctx        context.Context
	exec       *model.ImageTaskExecution
	rev        *model.ChannelRevision
	adapter    ImageTaskAdapter
	credential string
	now        int64
	adv        model.ImageTaskAdvance
}

func mergeProcessorSummary(dst *ImageTaskProcessorSummary, src ImageTaskProcessorSummary) {
	dst.ClaimLost += src.ClaimLost
	dst.Completed += src.Completed
	dst.Failed += src.Failed
	dst.Cancelled += src.Cancelled
	dst.ManualReview += src.ManualReview
	dst.StillInProgress += src.StillInProgress
}

func baseAdvance(exec *model.ImageTaskExecution, now int64, leaseOwner string) model.ImageTaskAdvance {
	return model.ImageTaskAdvance{
		ID: exec.ID, LeaseOwner: leaseOwner,
		ExpectedGeneration: exec.LeaseGeneration, Now: now,
		From: exec.State, To: exec.State,
	}
}

func markManualReview(summary *ImageTaskProcessorSummary, adv model.ImageTaskAdvance, reason string) {
	adv.To = adv.From
	won, err := model.MarkImageTaskManualReviewCAS(adv, reason)
	if err != nil {
		logger.LogWarn(context.Background(), fmt.Sprintf("image task processor: manual_review transition for execution %d failed: %v", adv.ID, err))
		return
	}
	if won {
		summary.ManualReview++
	}
}

// operationFromExecution maps the stored operation string onto the service-layer
// ImageOperation. Unknown persisted values fail closed before any provider
// request so a corrupt edit row can never be submitted as a generation.
func operationFromExecution(op string) (ImageOperation, error) {
	switch op {
	case string(ImageOperationGeneration):
		return ImageOperationGeneration, nil
	case string(ImageOperationEdit):
		return ImageOperationEdit, nil
	default:
		return "", fmt.Errorf("unsupported stored image operation %q", op)
	}
}

func (env *imageTaskProcessorEnv) stepContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(env.ctx, imageTaskProcessorStepTimeout)
}
