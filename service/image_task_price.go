package service

import (
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
)

// resolveImageTaskPrice builds the immutable price value object for one image
// task from the resolved pricing facts. The mode/source precedence mirrors
// ModelPriceHelperPerCall so an async image task prices identically to the
// synchronous per-call path (MJ / task):
//
//	model_price (explicit) -> default_model_price -> model_ratio
//
// A model with no configured price AND no configured ratio is a
// misconfiguration: the returned error wraps the pricing sentinel so the
// create orchestration maps it onto 500 INTERNAL_ERROR (fail closed, never a
// zero-charge guess). resolvedGroup is the concrete group the channel was
// selected from; userBaseGroup supplies the user×group ratio dimension and is
// the non-"auto" group even when selection expanded an auto group.
//
// otherRatios is always nil: the §5.2 price VO rejects non-empty other-ratio
// maps (Option A fail-closed), and image tasks have no multiplier axes beyond
// the per-unit price.
func resolveImageTaskPrice(originModel, userBaseGroup, resolvedGroup string) (*model.ImageTaskPriceResolution, error) {
	if price, ok := ratio_setting.GetModelPrice(originModel, false); ok {
		return newImageTaskPrice("model_price", "model_price", originModel, originModel,
			resolvedGroup, userBaseGroup, price, 0)
	}
	if defaultPrice, ok := ratio_setting.GetDefaultModelPriceMap()[originModel]; ok {
		return newImageTaskPrice("model_price", "default_model_price", originModel, originModel,
			resolvedGroup, userBaseGroup, defaultPrice, 0)
	}
	ratio, ratioOk, matchedName := ratio_setting.GetModelRatio(originModel)
	if !ratioOk {
		return nil, fmt.Errorf("%w: model %q has no price or ratio configured",
			model.ErrUnsupportedImageTaskPricingFacts, originModel)
	}
	return newImageTaskPrice("model_ratio", "model_ratio", originModel, matchedName,
		resolvedGroup, userBaseGroup, 0, ratio)
}

// newImageTaskPrice resolves the group ratio and constructs the validated VO.
// GetUserGroupRatio applies the same user-group × using-group lookup as the
// relay path: a configured group-group ratio wins, otherwise the using group's
// own ratio (default 1).
func newImageTaskPrice(mode, source, originModel, matchedModel, resolvedGroup, userBaseGroup string,
	modelPrice, modelRatio float64) (*model.ImageTaskPriceResolution, error) {
	groupRatio := GetUserGroupRatio(userBaseGroup, resolvedGroup)
	return model.NewImageTaskPriceResolution(
		mode, source, originModel, matchedModel, resolvedGroup,
		modelPrice, modelRatio, groupRatio, common.QuotaPerUnit,
		operation_setting.GetQuotaSetting().EnableFreeModelPreConsume,
		nil, // otherRatios: image tasks carry no extra multiplier axes (§5.2 fail-closed)
	)
}
