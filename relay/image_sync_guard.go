package relay

import (
	"fmt"
	"net/http"

	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
)

// WIS-580 (方案 1, dispatch fallback): relay.ImageHelper dispatches synchronous
// image requests through GetAdaptor(info.ApiType). An async-only image provider
// (APITypeApiNebula) has no entry in GetAdaptor's switch, so without this guard
// ImageHelper hits GetAdaptor==nil and returns a bare "invalid api type: 36" 500.
//
// This guard fail-closes that path with a clear, non-500 error pointing the
// caller at the async endpoint. It is the catch-all for entry points that bypass
// the model-layer candidate filter (specific-channel / affinity paths), which
// 方案 2 cannot reach.
//
// Returns nil when apiType may proceed through GetAdaptor — a sync-capable image
// adapter, or a non-image apiType whose GetAdaptor case exists (or which is left
// to the existing GetAdaptor-nil fail-close for genuinely unknown apiTypes).
func syncImageAdaptorGuard(apiType int, modelName string) *types.NewAPIError {
	if service.ApiTypeSupportsSyncImage(apiType) {
		return nil
	}
	return types.NewErrorWithStatusCode(
		fmt.Errorf("model %q is served by an async-only image provider; use /v1/image-tasks/* for this model", modelName),
		types.ErrorCodeInvalidApiType,
		http.StatusBadRequest,
		types.ErrOptionWithSkipRetry(),
	)
}
