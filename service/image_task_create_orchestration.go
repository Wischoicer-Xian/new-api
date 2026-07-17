package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

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
	RawBody                []byte
	Operation              ImageOperation
	OwnerUserID            int
	CreationTokenID        int
	IdempotencyKey         string
	UsingGroup             string
	UserBaseGroup          string
	RequestID              string
	Attribution            json.RawMessage // nil when no Wischoicer attribution is present
	TokenModelLimitEnabled bool
	TokenModelLimit        map[string]bool
	SpecificChannelID      int
	AcceptUnsetRatioModel  bool
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

	// Replay must not depend on mutable channel, capability, token-scope or
	// pricing configuration. The aggregate repeats this check under the owner
	// fence to converge concurrent first creates.
	replayExec, _, replayErr := model.FindImageTaskReplay(in.OwnerUserID, string(in.Operation), in.IdempotencyKey, hash)
	if replayErr != nil {
		if errors.Is(replayErr, model.ErrIdempotencyConflict) {
			return nil, false, 0, mapImageTaskReserveError(model.ErrImageTaskIdempotencyConflict)
		}
		return nil, false, 0, mapImageTaskReserveError(model.ErrImageTaskBillingData)
	}
	if replayExec != nil {
		return projectImageTaskObject(replayExec), true, 0, nil
	}

	if in.TokenModelLimitEnabled {
		matched := ratio_setting.FormatMatchingModelName(modelName)
		if in.TokenModelLimit == nil || !in.TokenModelLimit[matched] {
			return nil, false, 0, imageTaskReqError(dto.ImageTaskErrUnauthorized, 403,
				"creation token is not allowed to use this model")
		}
	}

	// Size guard: the current capability model carries no per-size support
	// matrix, so a present size cannot be proven supported and must be rejected
	// rather than silently dropped (§6.1). ApiNebula's async adapter will add a
	// size capability matrix; until then this stays capability-driven fail-closed.
	if hasSize {
		return nil, false, 0, imageTaskReqError(dto.ImageTaskErrUnsupportedParameter, 422,
			"size is not supported on this route")
	}

	resolvedGroup, selected, pErr := selectAndPickImageTaskChannel(in.UsingGroup, in.UserBaseGroup, modelName, in.Operation, in.SpecificChannelID)
	if pErr != nil {
		return nil, false, 0, pErr
	}

	price, prErr := resolveImageTaskPrice(modelName, in.UserBaseGroup, resolvedGroup, in.AcceptUnsetRatioModel)
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
		RequestData:       append(json.RawMessage(nil), in.RawBody...),
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

func selectAndPickImageTaskChannel(usingGroup, userBaseGroup, modelName string, op ImageOperation, specificChannelID int) (string, imageTaskChannelSelection, error) {
	groups := []string{usingGroup}
	if usingGroup == "auto" {
		groups = GetUserAutoGroup(userBaseGroup)
	}
	for _, group := range groups {
		candidates := model.ListImageCapableChannelsForGroupModel(group, modelName)
		if specificChannelID > 0 {
			filtered := candidates[:0]
			for _, candidate := range candidates {
				if candidate.Id == specificChannelID {
					filtered = append(filtered, candidate)
				}
			}
			candidates = filtered
		}
		valid := make([]imageTaskChannelSelection, 0, len(candidates))
		for _, candidate := range candidates {
			if selection, ok := trySelectImageTaskChannel(candidate, op, modelName); ok {
				valid = append(valid, selection)
			}
		}
		if len(valid) == 0 {
			continue
		}
		return group, weightedImageTaskSelection(valid, rand.Intn), nil
	}
	return "", imageTaskChannelSelection{}, imageTaskReqError(dto.ImageTaskErrServiceUnavailable, 503,
		"no image-capable channel available for model")
}

func weightedImageTaskSelection(candidates []imageTaskChannelSelection, draw func(int) int) imageTaskChannelSelection {
	highest := candidates[0].Channel.GetPriority()
	for _, candidate := range candidates[1:] {
		if candidate.Channel.GetPriority() > highest {
			highest = candidate.Channel.GetPriority()
		}
	}
	pool := make([]imageTaskChannelSelection, 0, len(candidates))
	total := 0
	for _, candidate := range candidates {
		if candidate.Channel.GetPriority() != highest {
			continue
		}
		pool = append(pool, candidate)
		weight := candidate.Channel.GetWeight()
		if weight > 0 {
			total += weight
		}
	}
	if total == 0 {
		total = len(pool) * 100
		index := draw(total) / 100
		return pool[index]
	}
	value := draw(total)
	for _, candidate := range pool {
		weight := candidate.Channel.GetWeight()
		if weight <= 0 {
			continue
		}
		value -= weight
		if value < 0 {
			return candidate
		}
	}
	return pool[len(pool)-1]
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
	_, ok = ImageAdapterVersion(apiType)
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
	if rev.AdapterVersion == "" {
		return imageTaskChannelSelection{}, false
	}
	frozenConfig, err := model.ImageExecutionConfigFromRevision(rev)
	if err != nil {
		return imageTaskChannelSelection{}, false
	}
	cfg, err := ParseImageChannelExecutionConfig(frozenConfig)
	if err != nil {
		return imageTaskChannelSelection{}, false
	}
	res, ok := ResolveImageExecution(caps, cfg, op, modelName)
	if !ok {
		return imageTaskChannelSelection{}, false
	}
	return imageTaskChannelSelection{
		Channel:        ch,
		Revision:       rev,
		Mode:           res.Mode,
		AdapterVersion: rev.AdapterVersion,
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
