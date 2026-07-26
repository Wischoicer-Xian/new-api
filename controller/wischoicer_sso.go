package controller

import (
	"net/http"
	"net/url"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

const (
	// wischoicerSsoStateCookie 是 /start 种下的浏览器绑定 state cookie（RFC v4 §2 N1/N5）。
	// callback 校验它与待完成 flow 对应——绑定发起浏览器、关 login-CSRF（攻击者把自己签发
	// 的 flow_token 塞进受害浏览器也无法在受害者浏览器命中此 cookie）。HttpOnly+Secure+
	// SameSite=Lax+Path=/+短 TTL（== flow TTL）。
	wischoicerSsoStateCookie = "wischoicer_sso_state"
	// wischoicerSsoFlowTTL 是 SSO flow 有效期（/start 建 flow + state cookie 都用它）。短窗口
	// 收敛重放面；callback 须在此窗口内消费。
	wischoicerSsoFlowTTL = 5 * time.Minute
)

// SsoStart handles GET /api/sso/wischoicer/start (RFC v4 §2 N1).
//
// Mount precondition: the route is registered only when common.WischoicerSsoEnabled=true
// (boot validation already guarantees origin + authorize-URL are valid). The handler:
//  1. creates an AuthFlow (purpose=wischoicer_sso, TTL=wischoicerSsoFlowTTL) → flow_token (F1);
//  2. sets a browser-binding state cookie (value=F1, HttpOnly+Secure+SameSite=Lax+Path=/+TTL);
//  3. 302 to ${WISCHOICER_SSO_AUTHORIZE_URL}?flow_token=F1 (authorize-URL boot-validated
//     HTTPS + fixed path; flow_token via url.URL builder, no string concat — §1.4).
//
// Accepts no user-supplied redirect/origin/target — callback landing is fixed (§1.4),
// closing the open-redirect surface.
func SsoStart(c *gin.Context) {
	flowToken, _, err := model.CreateAuthFlow(model.AuthFlowCreate{
		Purpose:   model.AuthFlowPurposeWischoicerSSO,
		Intent:    model.AuthFlowIntentLogin,
		ExpiresAt: time.Now().Add(wischoicerSsoFlowTTL),
	})
	if err != nil {
		common.SysError("wischoicer sso /start: create auth flow failed: " + err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "SSO start failed"})
		return
	}

	// state cookie 绑定发起浏览器（值=flow_token，callback 校验）。Secure=true 在 plain HTTP
	// 会被浏览器丢弃——有意为之：SSO 必须跑在 HTTPS 后（RFC §1.4），test-env 须先备 HTTPS/反代。
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(wischoicerSsoStateCookie, flowToken, int(wischoicerSsoFlowTTL.Seconds()), "/", "", true, true)

	// 302 到 authorize URL（boot-validated HTTPS + 固定 path）+ flow_token。用 url.URL builder，
	// 不字符串拼接（§1.4）。
	target, err := url.Parse(common.WischoicerSsoAuthorizeURL)
	if err != nil { // 不该发生（boot 校验过）——fail-closed
		common.SysError("wischoicer sso /start: authorize URL parse failed: " + err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "SSO start failed"})
		return
	}
	q := target.Query()
	q.Set("flow_token", flowToken)
	target.RawQuery = q.Encode()
	c.Redirect(http.StatusFound, target.String())
}
