package controller

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

const (
	// wischoicerSsoStateCookie 是 /start 种下的浏览器绑定 state cookie（RFC v4 §2 N1/N5 +
	// 记星 P1-1 三秘密）。cookie 值 = bsid（独立高熵，不离开 new-api），**不是 F1**——持有 F1
	// ≠ 持有 cookie。HttpOnly+Secure+SameSite=Lax+Path=wischoicerSsoStateCookiePath+短 TTL。
	wischoicerSsoStateCookie = "wischoicer_sso_state"
	// wischoicerSsoStateCookiePath 收窄到 /api/sso/wischoicer（RFC §4 line 178）：/start 与 callback
	// 同 path 前缀，收窄后两端收发 cookie 且不向其它路径泄漏。set 与 callback read 共用此常量。
	wischoicerSsoStateCookiePath = "/api/sso/wischoicer"
	// wischoicerSsoFlowTTL 是 SSO flow 有效期（F1 + state cookie 都用它）。短窗口收敛重放面。
	wischoicerSsoFlowTTL = 5 * time.Minute
	// wischoicerSsoBsidBytes 是 bsid（browser session id）的随机字节数（256-bit 高熵）。
	wischoicerSsoBsidBytes = 32
)

// SsoStart handles GET /api/sso/wischoicer/start (RFC v4 §2 N1 + 记星 P1-1 三秘密).
//
// Mount precondition: route registered only when common.WischoicerSsoEnabled=true (boot
// validation guarantees origin + authorize-URL valid + SessionCookieSecure=true).
//
// Three independent secrets (记星 P1-1): F1 (flow_token, in the 302 query), bsid (browser
// binding, in the state cookie; never leaves new-api except as the cookie value), and
// HMAC(bsid) bound into F1.payload (context binding, not empty). C2 (N4) carries the
// bsid_hash into F2; the callback (N5) verifies cookie(bsid) ↔ F2.payload in the F2-burn tx.
// Holding F1 ≠ holding the cookie (bsid), so a flow_token leaked/exfiltrated alone cannot
// complete SSO in another browser.
//
// Accepts no user-supplied redirect/origin/target — callback landing is fixed (§1.4).
func SsoStart(c *gin.Context) {
	bsid, err := generateWischoicerSsoBsid()
	if err != nil {
		common.SysError("wischoicer sso /start: generate bsid failed: " + err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "SSO start failed"})
		return
	}
	bsidHash := wischoicerSsoBsidHash(bsid)

	// F1：payload 绑定 bsid（HMAC），不再是空——C2 把 bsidHash 带入 F2，callback 据此校验浏览器。
	flowToken, _, err := model.CreateAuthFlow(model.AuthFlowCreate{
		Purpose:   model.AuthFlowPurposeWischoicerSSO,
		Intent:    model.AuthFlowIntentLogin,
		Payload:   bsidHash,
		ExpiresAt: time.Now().Add(wischoicerSsoFlowTTL),
	})
	if err != nil {
		common.SysError("wischoicer sso /start: create auth flow failed: " + err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "SSO start failed"})
		return
	}

	// state cookie 值 = bsid（**不是 F1**，三秘密）。Secure=true 在 plain HTTP 会被浏览器丢弃——
	// 有意为之（SSO 须 HTTPS，且 P1-2 已 boot 强制 SessionCookieSecure）；Path 收窄到 /api/sso/wischoicer。
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(wischoicerSsoStateCookie, bsid, int(wischoicerSsoFlowTTL.Seconds()), wischoicerSsoStateCookiePath, "", true, true)

	// 302 到 authorize URL（boot-validated HTTPS + 固定 path）+ flow_token=F1。url.URL builder，不拼字符串（§1.4）。
	// no-store 防缓存含 flow_token 的 302；no-referrer 防 Location 里的 flow_token 经 Referer 泄漏（RFC §4 line 178）。
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

// generateWischoicerSsoBsid 生成 wischoicerSsoBsidBytes-byte 高熵 browser session id（base64url）。
// 独立于 F1/F2，不离开 new-api（只作 cookie 值 + 经 HMAC 进 F1.payload）。
func generateWischoicerSsoBsid() (string, error) {
	b := make([]byte, wischoicerSsoBsidBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// wischoicerSsoBsidHash 算 bsid 的 context-binding HMAC（key 派生自 SessionSecret，与 auth_flow
// token hash 用不同的 domain-separation 前缀）。存 F1.payload；C2 带入 F2.payload；callback 恒时
// 校验 cookie(bsid) 经同式 HMAC 后 == F2.payload。
func wischoicerSsoBsidHash(bsid string) string {
	return common.GenerateHMACWithKey([]byte("wischoicer-sso-bsid:"+common.SessionSecret), bsid)
}
