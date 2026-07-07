package service

import (
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"

	"github.com/gin-gonic/gin"
)

// ─── WIS-460 preflight（跨服务只读额度预检，new-api 层）───
//
// 目的：在 content-workstation Step2 发车（置 references_generating）之前，让 wischoicer-user
// 门面调本接口问一次「这个用户按现行计费规则，能否启动一次 image2 生成」。纯读，绝不 reserve/decrease。
//
// 命门（记星 R2 review 绑定）：本文件只复用 helper.ModelPriceHelper（已被 controller/channel-test.go
// 无副作用调用）算 required_quota，并复刻 NewBillingSession 的 funding switch 只读判定。**绝不调用**
// PreConsumeBilling / NewBillingSession / BillingSession.preConsume / PreConsumeTokenQuota /
// DecreaseUserQuota / DecreaseTokenQuota / WalletFunding.PreConsume / SubscriptionFunding.PreConsume /
// PreConsumeUserSubscription。不变量测试见 quote_test.go（quote 前后 quota/token/subscription 恒等）。

// QuoteInput 是预检入参 = Step2 将要发给 image2 的「真实计费输入镜像」（记星 P1#1）。
// 至少 model + rendered prompt；size/quality 在价格依赖时一并传，由 ModelPriceHelper 同套 helper 算，
// 保证 billing mode 切 ratio/tiered_expr 时 required_quota 不漂。
type QuoteInput struct {
	Model   string
	Prompt  string
	Size    string
	Quality string
}

// QuoteResult 是预检契约（机读/人话分层，记星 P1#4）。
type QuoteResult struct {
	CanStart        bool   `json:"can_start"`
	Reason          string `json:"reason"`           // 稳定机读码：ok|insufficient_user_quota|model_price_not_configured|internal_error
	RequiredQuota   int    `json:"required_quota"`   // 机读 + 埋点
	CurrentQuota    int    `json:"current_quota"`    // 机读 + 埋点（wallet=用户额度；subscription=活跃订阅剩余合计）
	RequiredDisplay string `json:"required_display"` // 人话（前端直出）
	CurrentDisplay  string `json:"current_display"`  // 人话
	Message         string `json:"message"`          // 人话（前端直出）
	FundingSource   string `json:"funding_source"`   // wallet|subscription|""（记星 P1：只这两个）
}

// ComputePreflightQuote 纯读：算 required_quota + 按用户计费偏好只读判定 funding_source / 余额 / 是否够。
// 返回 *QuoteResult（始终非 nil，reason 填失败码）；调用方（controller）按 reason 决定 HTTP 状态。
func ComputePreflightQuote(c *gin.Context, userId int, in QuoteInput) *QuoteResult {
	res := &QuoteResult{Reason: "internal_error"}

	group, err := model.GetUserGroup(userId, false)
	if err != nil {
		res.Message = "无法读取用户分组"
		return res
	}
	userSetting, err := model.GetUserSetting(userId, false)
	if err != nil {
		res.Message = "无法读取用户设置"
		return res
	}

	// 真实计费输入镜像（记星 P1#1）。image2 用 ImageRequest.GetTokenCountMeta 算 ImagePriceRatio；
	// 若该模型走 tiered_expr，ModelPriceHelper 内部会用 BillingRequestInput（body 镜像）算表达式。
	imageReq := &dto.ImageRequest{Model: in.Model, Prompt: in.Prompt, Size: in.Size, Quality: in.Quality}
	info := &relaycommon.RelayInfo{
		UserId:          userId,
		UserGroup:       group,
		UsingGroup:      group,
		OriginModelName: in.Model,
		UserSetting:     userSetting,
		RequestHeaders:  map[string]string{},
	}
	// tiered_expr 的 param()/header() 可能读 body：喂镜像进去（无副作用，纯 marshal）。
	if reqInput, qerr := helper.BuildBillingExprRequestInputFromRequest(imageReq, nil); qerr == nil {
		info.BillingRequestInput = &reqInput
	}

	// ModelPriceHelper 只读（仅改内存 info.PriceData；channel-test.go:300 同样无副作用调用）。
	priceData, err := helper.ModelPriceHelper(c, info, 0, imageReq.GetTokenCountMeta())
	if err != nil {
		// 模型未配价格/比例 → fail-closed on pricing（配置缺失，非余额不足）。
		res.Reason = "model_price_not_configured"
		res.Message = fmt.Sprintf("模型 %s 未配置计费规则，请联系管理员", in.Model)
		return res
	}

	required := priceData.QuotaToPreConsume
	res.RequiredQuota = required
	res.RequiredDisplay = logger.FormatQuota(required)

	// 免费模型（required<=0）：无需额度，直接放行。
	if required <= 0 {
		res.CanStart = true
		res.Reason = "ok"
		res.CurrentDisplay = "—"
		res.Message = "免费模型，可直接生成"
		return res
	}

	pref := common.NormalizeBillingPreference(userSetting.BillingPreference)
	funding, current, canStart, reason := resolveFundingReadOnly(userId, pref, required)
	res.FundingSource = funding
	res.CurrentQuota = current
	res.CurrentDisplay = logger.FormatQuota(current)
	res.CanStart = canStart
	res.Reason = reason
	switch {
	case canStart:
		res.Message = "额度充足，可以生成"
	case reason == "insufficient_user_quota":
		res.Message = fmt.Sprintf("账户额度不足，需 %s，当前 %s，请充值或联系管理员后再试", res.RequiredDisplay, res.CurrentDisplay)
	default:
		res.Message = "无法判定额度，请稍后重试或联系管理员"
	}
	return res
}

// resolveFundingReadOnly 复刻 NewBillingSession 的 switch 决策（service/billing_session.go:401-441），
// 但**只走只读校验**（GetUserQuota / HasActiveUserSubscription / UserActiveSubscriptionsAllowWalletOverflow），
// 绝不 preConsume。返回 (funding_source, current, canStart, reason)。
//
// 与 NewBillingSession 同口径：
//   - wallet：userQuota>0 且 userQuota-required>=0 才够（与 tryWallet 的两道校验一致）。
//   - subscription：活跃订阅剩余合计 >= max(required,1) 才够（与 trySubscription 的 subConsume=max(preConsumed,1) 一致）。
func resolveFundingReadOnly(userId int, pref string, required int) (funding string, current int, canStart bool, reason string) {
	checkWallet := func() (string, int, bool, string) {
		q, err := model.GetUserQuota(userId, false)
		if err != nil {
			return "wallet", 0, false, "internal_error"
		}
		if q > 0 && q-required >= 0 {
			return "wallet", q, true, "ok"
		}
		return "wallet", q, false, "insufficient_user_quota"
	}
	checkSubscription := func() (string, int, bool, string) {
		subs, err := model.GetAllActiveUserSubscriptions(userId)
		if err != nil {
			return "subscription", 0, false, "internal_error"
		}
		var remaining int64
		for _, s := range subs {
			if s.Subscription != nil {
				remaining += s.Subscription.AmountTotal - s.Subscription.AmountUsed
			}
		}
		need := int64(required)
		if need < 1 {
			need = 1 // 对齐 trySubscription 的 subConsume=max(preConsumed,1)
		}
		if remaining >= need {
			return "subscription", int(remaining), true, "ok"
		}
		return "subscription", int(remaining), false, "insufficient_user_quota"
	}

	switch pref {
	case "wallet_only":
		return checkWallet()
	case "subscription_only":
		return checkSubscription()
	case "wallet_first":
		if f, cur, ok, r := checkWallet(); ok {
			return f, cur, ok, r
		}
		return checkSubscription() // 钱包不足 → 回退订阅（只读判定，不 preConsume）
	case "subscription_first":
		fallthrough
	default:
		hasSub, err := model.HasActiveUserSubscription(userId)
		if err != nil {
			return "subscription", 0, false, "internal_error"
		}
		if !hasSub {
			return checkWallet()
		}
		f, cur, ok, r := checkSubscription()
		if ok {
			return f, cur, ok, r
		}
		// 订阅不足：仅当活跃订阅允许钱包溢出时才回退钱包（与 NewBillingSession :429 一致）。
		allow, err := model.UserActiveSubscriptionsAllowWalletOverflow(userId)
		if err != nil {
			return f, cur, ok, r
		}
		if allow {
			return checkWallet()
		}
		return f, cur, ok, r
	}
}
