package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"

	"gorm.io/gorm"
)

// ImageTaskCreateInput carries every request-scoped fact CreateImageTask needs
// to reserve one image task. RawBody is the single source for both the strict
// decode and the canonical hash, so a re-read can never make them diverge. The
// caller resolves owner/token/group/request-id/attribution from the request
// context before invoking the orchestration.
//
// UsingGroup is the TokenAuth-resolved effective group (constant.ContextKeyUsingGroup);
// the literal "auto" means expand via GetUserAutoGroup. UserBaseGroup is the
// user's non-"auto" group (constant.ContextKeyUserGroup) and supplies the
// user×group ratio dimension as well as the auto-expansion input.
type ImageTaskCreateInput struct {
	RawBody         []byte
	Operation       ImageOperation
	OwnerUserID     int
	CreationTokenID int
	IdempotencyKey  string
	UsingGroup      string
	UserBaseGroup   string
	RequestID       string
	Attribution     json.RawMessage // nil when no Wischoicer attribution is present
}

// imageTaskChannelSelection is the resolved channel a create will reserve
// against: the channel, its latest immutable revision, the resolved execution
// mode, and the adapter implementation version frozen into the execution row.
type imageTaskChannelSelection struct {
	Channel        *model.Channel
	Revision       *model.ChannelRevision
	Mode           ImageExecutionMode
	AdapterVersion string
}

// CreateImageTask orchestrates the §6.1 create path: strict decode + size
// guard, canonical hash, group resolution, image-capable channel selection,
// capability/revision/price resolution, then the atomic reserve aggregate. It
// returns the public projection for a 202 Accepted response, a replayed flag
// (drives the Idempotency-Replayed header), and a Retry-After hint (non-zero
// only on mapped 429).
//
// Selection is single-shot (§7.5: no Distribute retry / channel switching):
// candidates are enumerated once and the first one that clears capability,
// revision and price resolution is chosen. Exhausting every candidate is a
// fail-closed 503, never a silent fallback.
func CreateImageTask(ctx context.Context, in ImageTaskCreateInput) (obj *dto.ImageTaskObject, replayed bool, retryAfter int, err error) {
	modelName, hasSize, hash, dErr := decodeAndHash(in.Operation, in.RawBody)
	if dErr != nil {
		return nil, false, 0, dErr
	}

	// Size guard: the current capability model carries no per-size support
	// matrix, so a present size cannot be proven supported and must be rejected
	// rather than silently dropped (§6.1). ApiNebula's async adapter will add a
	// size capability matrix; until then this stays capability-driven fail-closed.
	if hasSize {
		return nil, false, 0, imageTaskReqError(dto.ImageTaskErrUnsupportedParameter, 422,
			"size is not supported on this route")
	}

	resolvedGroup, candidates, selErr := selectImageTaskCandidates(in.UsingGroup, in.UserBaseGroup, modelName)
	if selErr != nil {
		return nil, false, 0, selErr
	}

	selected, pErr := pickImageTaskChannel(candidates, in.Operation, modelName)
	if pErr != nil {
		return nil, false, 0, pErr
	}

	price, prErr := resolveImageTaskPrice(modelName, in.UserBaseGroup, resolvedGroup)
	if prErr != nil {
		return nil, false, 0, mapImageTaskReserveError(prErr)
	}

	cmd := model.ImageTaskReserveCommand{
		OwnerUserID:       in.OwnerUserID,
		Operation:         string(in.Operation),
		IdempotencyKey:    in.IdempotencyKey,
		RequestHash:       hash,
		ChannelRevisionID: selected.Revision.ID,
		ExecutionMode:     string(selected.Mode),
		AdapterVersion:    selected.AdapterVersion,
		CreationTokenID:   in.CreationTokenID,
		Price:             price,
		Attribution:       in.Attribution,
		RequestID:         in.RequestID,
		UpstreamRequestID: "", // §7.4: upstream id is filled by the processor at submit
		Now:               time.Now().Unix(),
	}
	outcome, err := model.ReserveImageTask(ctx, cmd)
	if err != nil {
		return nil, false, 0, mapImageTaskReserveError(err)
	}
	return projectImageTaskObject(outcome.Execution), outcome.Replayed, 0, nil
}

// decodeAndHash strict-decodes the raw body for one operation and computes the
// canonical request hash from the SAME bytes. Returning the model name and the
// size-present flag lets the orchestration apply the size guard and drive
// channel/price resolution without re-decoding.
func decodeAndHash(operation ImageOperation, rawBody []byte) (modelName string, hasSize bool, hash string, err error) {
	switch operation {
	case ImageOperationGeneration:
		req, dErr := dto.DecodeImageTaskGenerationRequest(rawBody)
		if dErr != nil {
			return "", false, "", dErr
		}
		modelName, hasSize = req.Model, req.Size != nil
	case ImageOperationEdit:
		req, dErr := dto.DecodeImageTaskEditRequest(rawBody)
		if dErr != nil {
			return "", false, "", dErr
		}
		modelName, hasSize = req.Model, req.Size != nil
	default:
		return "", false, "", imageTaskReqError(dto.ImageTaskErrInvalidRequest, 400, "unsupported image operation")
	}
	hash, err = CanonicalImageRequestHash(rawBody)
	if err != nil {
		return "", false, "", imageTaskReqError(dto.ImageTaskErrInvalidRequest, 400, "malformed request body")
	}
	return modelName, hasSize, hash, nil
}

// selectImageTaskCandidates resolves the effective group and enumerates its
// image-capable candidates for the model. "auto" expands into the user's auto
// group list and picks the FIRST group with candidates (resolvedGroup becomes
// that concrete group for ratio/fingerprint); a concrete group is tried
// directly. There is no cross-group fan-out — the image task path must not
// Distribute (§7.5). Candidates are sorted by priority before returning so the
// picker is deterministic.
func selectImageTaskCandidates(usingGroup, userBaseGroup, modelName string) (string, []*model.Channel, error) {
	groupsToTry := []string{usingGroup}
	if usingGroup == "auto" {
		groupsToTry = GetUserAutoGroup(userBaseGroup)
	}
	for _, g := range groupsToTry {
		raw := model.ListImageCapableChannelsForGroupModel(g, modelName)
		if len(raw) == 0 {
			continue
		}
		return g, sortImageTaskCandidates(raw), nil
	}
	return "", nil, imageTaskReqError(dto.ImageTaskErrServiceUnavailable, 503,
		"no image-capable channel available for model")
}

// sortImageTaskCandidates returns a priority-descending copy of candidates,
// breaking ties by ascending channel id for stable, testable selection. The
// input slice is not mutated.
func sortImageTaskCandidates(candidates []*model.Channel) []*model.Channel {
	sorted := make([]*model.Channel, len(candidates))
	copy(sorted, candidates)
	sort.SliceStable(sorted, func(i, j int) bool {
		pi, pj := sorted[i].GetPriority(), sorted[j].GetPriority()
		if pi != pj {
			return pi > pj
		}
		return sorted[i].Id < sorted[j].Id
	})
	return sorted
}

// pickImageTaskChannel walks the sorted candidates and returns the first one
// that clears the adapter capability, execution-config, execution-resolution
// and revision gates. Any candidate that fails one gate is skipped; exhausting
// the list is a fail-closed 503.
func pickImageTaskChannel(candidates []*model.Channel, op ImageOperation, modelName string) (imageTaskChannelSelection, error) {
	for _, ch := range candidates {
		if sel, ok := trySelectImageTaskChannel(ch, op, modelName); ok {
			return sel, nil
		}
	}
	return imageTaskChannelSelection{}, imageTaskReqError(dto.ImageTaskErrServiceUnavailable, 503,
		"no image-capable channel available for model")
}

// trySelectImageTaskChannel resolves one candidate to a selection. The bool is
// false when the channel cannot serve this operation+model (unsupported
// adapter, unparseable config, unsupported mode, or no frozen revision). A
// missing revision (gorm.ErrRecordNotFound) is a consistency violation for an
// image-configured channel and is treated as skip; other DB errors are logged
// and also skipped so a transient failure fails closed rather than 500-ing on
// a single channel.
func trySelectImageTaskChannel(ch *model.Channel, op ImageOperation, modelName string) (imageTaskChannelSelection, bool) {
	apiType, typeOK := common.ChannelType2APIType(ch.Type)
	if !typeOK {
		return imageTaskChannelSelection{}, false
	}
	caps, ok := ImageAdapterCapabilities(apiType)
	if !ok || caps == nil {
		return imageTaskChannelSelection{}, false
	}
	adapterVersion, ok := ImageAdapterVersion(apiType)
	if !ok {
		return imageTaskChannelSelection{}, false
	}
	cfg, err := ParseImageChannelExecutionConfig(ch.ImageExecutionConfigBytes())
	if err != nil {
		return imageTaskChannelSelection{}, false
	}
	res, ok := ResolveImageExecution(caps, cfg, op, modelName)
	if !ok {
		return imageTaskChannelSelection{}, false
	}
	rev, err := model.GetLatestChannelRevisionByChannelID(ch.Id)
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			common.SysError(fmt.Sprintf("image task create: load revision for channel %d: %v", ch.Id, err))
		}
		return imageTaskChannelSelection{}, false
	}
	return imageTaskChannelSelection{
		Channel:        ch,
		Revision:       rev,
		Mode:           res.Mode,
		AdapterVersion: adapterVersion,
	}, true
}

// mapImageTaskReserveError maps the reserve aggregate's sentinel errors onto
// the public §6.1 error codes. Unknown errors are returned unchanged so the
// controller's writeImageTaskError reports them as a generic 500 rather than
// leaking an internal message.
func mapImageTaskReserveError(err error) error {
	switch {
	case errors.Is(err, model.ErrImageTaskIdempotencyConflict):
		return imageTaskReqError(dto.ImageTaskErrIdempotencyConflict, 409,
			"idempotency key reused with a different request")
	case errors.Is(err, model.ErrImageTaskInFlightCapReached):
		e := imageTaskReqError(dto.ImageTaskErrTooManyRequests, 429,
			"per-user in-flight image task cap reached")
		e.RetryAfter = 1
		return e
	case errors.Is(err, model.ErrImageTaskWalletInsufficient),
		errors.Is(err, model.ErrImageTaskSubscriptionInsufficient),
		errors.Is(err, model.ErrImageTaskNoActiveSubscription):
		return imageTaskReqError(dto.ImageTaskErrInsufficientQuota, 402,
			"insufficient quota for image task")
	case errors.Is(err, model.ErrImageTaskTokenInvalid):
		return imageTaskReqError(dto.ImageTaskErrUnauthorized, 401,
			"creation token is invalid")
	case errors.Is(err, model.ErrUnsupportedImageTaskPricingFacts):
		return imageTaskReqError(dto.ImageTaskErrInternal, 500,
			"image task pricing is misconfigured")
	case errors.Is(err, model.ErrImageTaskBillingData),
		errors.Is(err, model.ErrImageTaskBillingRetryExhausted):
		return imageTaskReqError(dto.ImageTaskErrInternal, 500,
			"image task billing error")
	case errors.Is(err, model.ErrImageTaskCacheSafetyMisconfigured):
		return imageTaskReqError(dto.ImageTaskErrServiceUnavailable, 503,
			"image task cache safety misconfigured")
	}
	return err
}

// imageTaskReqError is the service-layer constructor for a public image-task
// request error. dto.imageTaskError is unexported, so the orchestration builds
// the value object directly via this helper.
func imageTaskReqError(code dto.ImageTaskErrorCode, status int, msg string) *dto.ImageTaskRequestError {
	return &dto.ImageTaskRequestError{Code: code, StatusCode: status, Message: msg}
}
