package helper

import (
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/pricing"
	"github.com/QuantumNous/new-api/relaykit/types"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

func HandleGroupRatio(ctx *gin.Context, info *relaycommon.RelayInfo) hosttypes.GroupRatioInfo {
	return pricing.HandleGroupRatio(ctx, info)
}

func ModelPriceHelper(ctx *gin.Context, info *relaycommon.RelayInfo, promptTokens int, meta *types.TokenCountMeta) (hosttypes.PriceData, error) {
	return pricing.ModelPriceHelper(ctx, info, promptTokens, meta)
}

func ModelPriceHelperPerCall(ctx *gin.Context, info *relaycommon.RelayInfo) (hosttypes.PriceData, error) {
	return pricing.ModelPriceHelperPerCall(ctx, info)
}

func HasModelBillingConfig(modelName string) bool {
	return pricing.HasModelBillingConfig(modelName)
}

func HasPriceOrRatioEntry(modelName string) bool {
	return pricing.HasPriceOrRatioEntry(modelName)
}
