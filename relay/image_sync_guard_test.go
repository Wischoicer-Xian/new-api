package relay

import (
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/assert"
)

// WIS-580 (方案 1, dispatch fallback): when a sync image request reaches
// relay.ImageHelper with an async-only apiType (APITypeApiNebula) — e.g. via the
// specific-channel path that bypasses candidate filtering — the dispatch guard
// must fail-close with a clear, non-500 error pointing at /v1/image-tasks/*,
// instead of GetAdaptor==nil producing a bare "invalid api type: 36" 500.
//
// P2: operation-granular via the path — an apiType that is sync for generation
// but async for edit must be rejected on the edit path.
func TestSyncImageAdaptorGuard(t *testing.T) {
	// Async-only image apiType on either sync image path -> fail-close.
	err := syncImageAdaptorGuard(constant.APITypeApiNebula, "/v1/images/generations", "gpt-image-2")
	assert.NotNil(t, err, "async-only apiType must be fail-closed at dispatch (WIS-580 方案 1)")
	assert.Equal(t, http.StatusBadRequest, err.StatusCode, "must be a clear non-500, not the bare GetAdaptor-nil 500")
	assert.True(t, types.IsSkipRetryError(err), "must skip retry — no sync adaptor can serve it")
	assert.True(t, strings.Contains(err.Error(), "image-tasks"), "error must point the caller at the async endpoint")
	assert.True(t, strings.Contains(err.Error(), "gpt-image-2"), "error must name the model")

	// Edit path also fail-closes for async-only.
	assert.NotNil(t, syncImageAdaptorGuard(constant.APITypeApiNebula, "/v1/images/edits", "gpt-image-2"))

	// Sync-capable image apiType and non-image apiTypes pass through (nil).
	assert.Nil(t, syncImageAdaptorGuard(constant.APITypeOpenAI, "/v1/images/generations", "gpt-image-1"))
	assert.Nil(t, syncImageAdaptorGuard(constant.APITypeOpenAI, "/v1/images/edits", "gpt-image-1"))
	assert.Nil(t, syncImageAdaptorGuard(constant.APITypeAnthropic, "/v1/images/generations", "claude-3"),
		"non-image apiType has its own GetAdaptor case and must pass")
}
