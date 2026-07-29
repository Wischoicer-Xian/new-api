package service

import (
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"

	"github.com/gin-gonic/gin"
)

// ─── WIS-460 preflight（跨服务只读额度预检，new-api 层）───
//
// 目的：在 content-workstation Step2 发车（置 references_generating）之前，让 wischoicer-user
// 门面调本接口问一次「这个用户按现行计费规则，能否启动一次 image2 生成」。纯读，绝不 reserve/decrease。
//
// 命门（记星 R2 review 绑定）：预检口径必须 == 真实预扣口径。本文件只复用 helper.ModelPriceHelper
// （已被 controller/channel-test.go 无副作用调用）算 required_quota，并按 PreConsumeUserSubscription
// 的真实单订阅语义判定 funding；**绝不调用** PreConsumeBilling / NewBillingSession / preConsume /
// PreConsumeTokenQuota / DecreaseUserQuota / DecreaseTokenQuota / WalletFunding.PreConsume /
// SubscriptionFunding.PreConsume / PreConsumeUserSubscription。不变量测试见 quote_test.go。

// QuoteInput 是预检入参 = Step2 将要发给 image2 的「真实计费输入镜像」（记星 P1#1 + R2 P1#2）。
// Body/Headers 非空时原样作为 tiered_expr 的 RequestInput（防 billing mode 切 body/header 依赖时
// required_quota 漂）；空时退回用 Model/Prompt/Size/Quality 构造 ImageRequest。
type QuoteInput struct {
	Model   string
	Prompt  string
	Size    string
	Quality string
	// Body 是真实 image2 请求体镜像；非空时优先解析它得 prompt/size/quality（算 ImagePriceRatio）。
	Body []byte
	// Headers 是真实 header 镜像；tiered_expr 的 header()/param() 可能读，不可传 nil 截断。
	Headers map[string]string
}

// QuoteResult 是预检契约（机读/人话分层，记星 P1#4）。
type QuoteResult struct {
	CanStart        bool   `json:"can_start"`
	Reason          string `json:"reason"`           // 稳定机读码：ok|insufficient_user_quota|model_price_not_configured|internal_error
	RequiredQuota   int    `json:"required_quota"`   // 机读 + 埋点
	CurrentQuota    int    `json:"current_quota"`    // 机读 + 埋点（wallet=用户额度；subscription=单张可覆盖订阅的剩余，无限订阅=required）
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

	// 真实计费输入镜像（记星 P1#1 + R2 P1#2）：Body 非空 → 从真实请求体解析 ImageRequest；
	// 否则退回 Model/Prompt/Size/Quality 构造。model 字段为镜像里的真实模型。
	var imageReq *dto.ImageRequest
	if len(in.Body) > 0 {
		imageReq = &dto.ImageRequest{}
		if err := common.Unmarshal(in.Body, imageReq); err != nil {
			res.Message = "无法解析 billing body 镜像: " + err.Error()
			return res
		}
	} else {
		imageReq = &dto.ImageRequest{Model: in.Model, Prompt: in.Prompt, Size: in.Size, Quality: in.Quality}
	}

	headers := in.Headers
	if headers == nil {
		headers = map[string]string{}
	}
	info := &relaycommon.RelayInfo{
		UserId:          userId,
		UserGroup:       group,
		UsingGroup:      group,
		OriginModelName: imageReq.Model,
		UserSetting:     userSetting,
		RequestHeaders:  headers,
	}
	// tiered_expr 的 param()/header() 读真实 body + headers（记星 R2 P1#2：不可 nil 截断）。
	// Body 非空原样喂；空时用 ImageRequest marshal 一份（纯 marshal，无副作用）。
	bodyBytes := in.Body
	if len(bodyBytes) == 0 {
		if b, merr := common.Marshal(imageReq); merr == nil {
			bodyBytes = b
		}
	}
	reqInput := billingexpr.RequestInput{Headers: headers, Body: bodyBytes}
	info.BillingRequestInput = &reqInput

	// ModelPriceHelper 只读（仅改内存 info.PriceData；channel-test.go:300 同样无副作用调用）。
	priceData, err := helper.ModelPriceHelper(c, info, 0, imageReq.GetTokenCountMeta())
	if err != nil {
		// 模型未配价格/比例 → fail-closed on pricing（配置缺失，非余额不足）。
		res.Reason = "model_price_not_configured"
		res.Message = fmt.Sprintf("模型 %s 未配置计费规则，请联系管理员", imageReq.Model)
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
	funding, current, canStart, reason, unlimited := resolveFundingReadOnly(userId, pref, required)
	res.FundingSource = funding
	res.CurrentQuota = current
	if unlimited {
		res.CurrentDisplay = "不限（订阅）"
	} else {
		res.CurrentDisplay = logger.FormatQuota(current)
	}
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
// 但**只走只读校验**，绝不 preConsume。返回 (funding_source, current, canStart, reason, unlimited)。
//
// 命门（记星 R2 P1#1）—— 订阅口径与真实 PreConsumeUserSubscription 对齐（model/subscription.go:1190-1238）：
// 真实预扣锁**单张**活跃订阅，要求单张 remain >= amount，**不跨订阅求和**；AmountTotal<=0 的无限订阅
// 跳过 remain 校验（始终覆盖）。故 can_start(subscription) = 存在任一活跃订阅覆盖（无限 或 单张剩余>=need）。
//
// wallet 口径与 tryWallet 一致：userQuota>0 且 userQuota-required>=0。
func resolveFundingReadOnly(userId int, pref string, required int) (funding string, current int, canStart bool, reason string, unlimited bool) {
	need := int64(required)
	if need < 1 {
		need = 1 // 对齐 trySubscription 的 subConsume=max(preConsumed,1)
	}

	checkWallet := func() (string, int, bool, string, bool) {
		q, err := model.GetUserQuota(userId, false)
		if err != nil {
			return "wallet", 0, false, "internal_error", false
		}
		if q > 0 && q-required >= 0 {
			return "wallet", q, true, "ok", false
		}
		return "wallet", q, false, "insufficient_user_quota", false
	}
	// checkSubscription 按 PreConsumeUserSubscription 真实语义：单张覆盖，不求和；无限订阅覆盖。
	checkSubscription := func() (string, int, bool, string, bool) {
		subs, err := model.GetAllActiveUserSubscriptions(userId)
		if err != nil {
			return "subscription", 0, false, "internal_error", false
		}
		var bestRemain int64 // display 用：最佳单张剩余
		covered := false
		hasUnlimited := false
		for _, s := range subs {
			if s.Subscription == nil {
				continue
			}
			sub := s.Subscription
			if sub.AmountTotal <= 0 {
				// 无限订阅：与 PreConsumeUserSubscription 一致，跳过 remain 校验，直接覆盖。
				hasUnlimited = true
				covered = true
				continue
			}
			remain := sub.AmountTotal - sub.AmountUsed
			if remain > bestRemain {
				bestRemain = remain
			}
			if remain >= need {
				covered = true // 单张覆盖即可（真实预扣锁这张）
			}
		}
		if covered {
			cur := int(bestRemain)
			if hasUnlimited {
				cur = required // 无限：current>=required 成立，display 标「不限」
			}
			return "subscription", cur, true, "ok", hasUnlimited
		}
		return "subscription", int(bestRemain), false, "insufficient_user_quota", false
	}

	switch pref {
	case "wallet_only":
		return checkWallet()
	case "subscription_only":
		return checkSubscription()
	case "wallet_first":
		if f, cur, ok, r, un := checkWallet(); ok {
			return f, cur, ok, r, un
		}
		return checkSubscription() // 钱包不足 → 回退订阅（只读判定，不 preConsume）
	case "subscription_first":
		fallthrough
	default:
		hasSub, err := model.HasActiveUserSubscription(userId)
		if err != nil {
			return "subscription", 0, false, "internal_error", false
		}
		if !hasSub {
			return checkWallet()
		}
		f, cur, ok, r, un := checkSubscription()
		if ok {
			return f, cur, ok, r, un
		}
		// 订阅不足：仅当活跃订阅允许钱包溢出时才回退钱包（与 NewBillingSession :429 一致）。
		allow, err := model.UserActiveSubscriptionsAllowWalletOverflow(userId)
		if err != nil {
			return f, cur, ok, r, un
		}
		if allow {
			return checkWallet()
		}
		return f, cur, ok, r, un
	}
}
