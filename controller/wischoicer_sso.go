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
	// SameSite=Lax+Path=wischoicerSsoStateCookiePath+短 TTL（== flow TTL）。
	wischoicerSsoStateCookie = "wischoicer_sso_state"
	// wischoicerSsoStateCookiePath 收窄到 /api/sso/wischoicer（RFC v4 §4 line 178）：/start
	//（/api/sso/wischoicer/start）与 callback（/api/sso/wischoicer/callback）同 path 前缀，
	// 收窄后两端都能收发 cookie 且不向其它路径泄漏。记星 review 若改 "/"，flip 此常量即可——
	// /start set 与未来 callback read 共用，一处改两处对齐（张驰 N5 note）。
	wischoicerSsoStateCookiePath = "/api/sso/wischoicer"
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
	// Path 收窄到 /api/sso/wischoicer（RFC §4 line 178），callback 同前缀能收。
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(wischoicerSsoStateCookie, flowToken, int(wischoicerSsoFlowTTL.Seconds()), wischoicerSsoStateCookiePath, "", true, true)

	// 302 到 authorize URL（boot-validated HTTPS + 固定 path）+ flow_token。用 url.URL builder，
	// 不字符串拼接（§1.4）。no-store 防缓存含 flow_token 的 302；no-referrer 防 Location 里的
	// flow_token 经 Referer 泄漏到 authorize 下一跳（RFC §4 line 178）。
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
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
