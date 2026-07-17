package service

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// saveRestorePriceMaps snapshots the live model price/ratio maps and restores
// them at the end of the test, so resolveImageTaskPrice tests can rewrite the
// maps without bleeding into other tests.
func saveRestorePriceMaps(t *testing.T) {
	t.Helper()
	savedPrice := ratio_setting.GetModelPriceCopy()
	savedRatio := ratio_setting.GetModelRatioCopy()
	t.Cleanup(func() {
		if b, err := common.Marshal(savedPrice); err == nil {
			_ = ratio_setting.UpdateModelPriceByJSONString(string(b))
		}
		if b, err := common.Marshal(savedRatio); err == nil {
			_ = ratio_setting.UpdateModelRatioByJSONString(string(b))
		}
	})
}

func TestResolveImageTaskPrice_ModeSourcePrecedence(t *testing.T) {
	saveRestorePriceMaps(t)

	t.Run("explicit model_price wins", func(t *testing.T) {
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"img-explicit":0.02}`))
		v, err := resolveImageTaskPrice("img-explicit", "default", "default")
		require.NoError(t, err)
		assert.True(t, v.IsFixedMode())
		assert.Equal(t, "model_price", v.PricingSourceRaw())
		assert.Equal(t, 0.02, v.ModelPrice())
		assert.Equal(t, "default", v.ResolvedGroup())
	})

	t.Run("default_model_price fallback when live map misses", func(t *testing.T) {
		// Clearing the live map exposes the built-in default map; dall-e-3 is
		// priced there (0.04) but absent from the now-empty live map.
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{}`))
		v, err := resolveImageTaskPrice("dall-e-3", "default", "default")
		require.NoError(t, err)
		assert.True(t, v.IsFixedMode())
		assert.Equal(t, "default_model_price", v.PricingSourceRaw())
		assert.Equal(t, 0.04, v.ModelPrice())
	})

	t.Run("model_ratio when neither live nor default price hits", func(t *testing.T) {
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{}`))
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"img-ratio":2.5}`))
		v, err := resolveImageTaskPrice("img-ratio", "default", "vip")
		require.NoError(t, err)
		assert.True(t, v.IsRatioMode())
		assert.Equal(t, "model_ratio", v.PricingSourceRaw())
		assert.Equal(t, 2.5, v.ModelRatio())
		assert.Equal(t, "vip", v.ResolvedGroup())
	})

	t.Run("unconfigured model fails closed with pricing sentinel", func(t *testing.T) {
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{}`))
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{}`))
		_, err := resolveImageTaskPrice("no-such-model", "default", "default")
		require.Error(t, err)
		assert.ErrorIs(t, err, model.ErrUnsupportedImageTaskPricingFacts)
	})
}

func TestResolveImageTaskPrice_GroupRatioFallback(t *testing.T) {
	saveRestorePriceMaps(t)
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"img-g":0.02}`))

	// No group-group or group ratio configured: GetUserGroupRatio falls back to
	// GetGroupRatio, whose default is 1. This locks the fallback chain that
	// keeps the frozen fingerprint's group ratio consistent with the relay path.
	v, err := resolveImageTaskPrice("img-g", "default", "default")
	require.NoError(t, err)
	assert.Equal(t, 1.0, v.GroupRatio())
}
