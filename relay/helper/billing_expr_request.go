package helper

import (
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/pricing"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
)

func ResolveIncomingBillingExprRequestInput(ctx *gin.Context, info *relaycommon.RelayInfo, readsBody bool) (billingexpr.RequestInput, error) {
	return pricing.ResolveIncomingBillingExprRequestInput(ctx, info, readsBody)
}

func ResolveImageBillingRequestInput(ctx *gin.Context, info *relaycommon.RelayInfo, input billingexpr.RequestInput) (billingexpr.RequestInput, error) {
	return pricing.ResolveImageBillingRequestInput(ctx, info, input)
}

func BuildBillingExprRequestInputFromRequest(request dto.Request, headers map[string]string) (billingexpr.RequestInput, error) {
	return pricing.BuildBillingExprRequestInputFromRequest(request, headers)
}
