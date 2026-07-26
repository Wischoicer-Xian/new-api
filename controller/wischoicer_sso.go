package controller

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const (
	// wischoicerSsoStateCookie 是 /start 种下的浏览器绑定 state cookie（RFC v4 §2 N1/N5 +
	// 记星 P1-1 三秘密）。cookie 值 = bsid（独立高熵，不离开 new-api），**不是 F1**——持有 F1
	// ≠ 持有 cookie。HttpOnly+Secure+SameSite=Lax+Path=wischoicerSsoStateCookiePath+短 TTL。
	wischoicerSsoStateCookie = "wischoicer_sso_state"
	// wischoicerSsoStateCookiePath 收窄到 /api/sso/wischoicer（RFC §4 line 178）：/start 与 callback
	// 同 path 前缀，收窄后两端收发 cookie 且不向其它路径泄漏。set / read / clear 共用此常量（张驰 N5 note）。
	wischoicerSsoStateCookiePath = "/api/sso/wischoicer"
	// wischoicerSsoFlowTTL 是 SSO flow 有效期（F1 + F2 + state cookie 都用它）。短窗口收敛重放面。
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
// HMAC(bsid) bound into F1.payload (context binding, not empty). C2 (SsoAuthorize) copies
// F1.payload verbatim into F2; SsoCallback verifies cookie(bsid) ↔ F2.payload in the F2-burn
// tx. Holding F1 ≠ holding the cookie (bsid), so a flow_token leaked/exfiltrated alone cannot
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

	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(wischoicerSsoStateCookie, bsid, int(wischoicerSsoFlowTTL.Seconds()), wischoicerSsoStateCookiePath, "", true, true)

	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	target, err := url.Parse(common.WischoicerSsoAuthorizeURL)
	if err != nil {
		common.SysError("wischoicer sso /start: authorize URL parse failed: " + err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "SSO start failed"})
		return
	}
	q := target.Query()
	q.Set("flow_token", flowToken)
	target.RawQuery = q.Encode()
	c.Redirect(http.StatusFound, target.String())
}

// wischoicerSsoAuthorizeRequest 是 user-service broker (C3) 调 C2 的请求体。
type wischoicerSsoAuthorizeRequest struct {
	FlowToken    string `json:"flow_token" binding:"required"`
	NewApiUserId int    `json:"new_api_user_id" binding:"required"`
}

// SsoAuthorize (C2) handles POST /api/sso/wischoicer/authorize (RFC v4 §2 N4-C2 + 记星 edec1c4b).
//
// Mount precondition: WischoicerSsoInternalAuth (sso-service-token) + WischoicerSsoEnabled gate.
// user-service broker 调此端点：消费 F1（/start 的 flow）＋ **在同一事务** mint F2（one-time login
// code，F1.payload 原样复制进 F2.payload，不解码/不重算——记星 edec1c4b），返回 callback_url。
// F1 一次性消费、不可重放；F2 由 callback 消费建 session。
func SsoAuthorize(c *gin.Context) {
	var req wischoicerSsoAuthorizeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid request"})
		return
	}
	var callbackURL string
	_, err := model.ConsumeAuthFlowWithAction(req.FlowToken, model.AuthFlowMatch{Purpose: model.AuthFlowPurposeWischoicerSSO}, func(tx *gorm.DB, f1 *model.AuthFlow) error {
		f2Token, _, err := model.CreateAuthFlowWithTx(tx, model.AuthFlowCreate{
			Purpose:   model.AuthFlowPurposeWischoicerSSO,
			Intent:    model.AuthFlowIntentLogin,
			UserId:    req.NewApiUserId,
			Payload:   f1.Payload, // 记星 edec1c4b：原样复制，不解码/不重算
			ExpiresAt: time.Now().Add(wischoicerSsoFlowTTL),
		})
		if err != nil {
			return err
		}
		callbackURL = common.WischoicerSsoCallbackURL(f2Token)
		return nil
	})
	if err != nil {
		// F1 无效/过期/已消费（不可重放）——不泄露具体原因。
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid flow"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "callback_url": callbackURL})
}

// SsoCallback (N5) handles GET /api/sso/wischoicer/callback?code=F2 (RFC v4 §2 N5 + 记星 edec1c4b).
//
// 浏览器从 user-service 302 回此端点（带 state cookie）。在消费 F2 的同事务内恒时校验 bsid
//（cookie(bsid) → HMAC → 比 F2.payload）。**cookie 缺失 / 格式错 / hash 不符都先提交 F2 消费、再返回
// 统一失败**（记星 edec1c4b：失败也提交，防 bsid 爆破——每次错探都烧一个 F2）。匹配则 CreateLoginSession
// + refresh cookie → /dashboard；否则 → /login（统一失败，不泄原因）。
func SsoCallback(c *gin.Context) {
	code := c.Query("code")
	bsid, _ := c.Cookie(wischoicerSsoStateCookie)
	clearWischoicerSsoStateCookie(c) // one-shot：无论成败都清（set/read/clear 同 Path 对称）

	var (
		matched bool
		userID  int
	)
	// 在消费 F2 的事务内恒时校验 bsid——action 始终返回 nil（**失败也提交**：cookie 缺失 / 格式错 /
	// hash 不符都先提交 F2 消费、再统一失败，记星 edec1c4b——每次错探都烧一个 F2）。session 创建移到
	// 事务外，避免 ConsumeAuthFlowWithAction 事务内再开 CreateLoginSession 写事务→sqlite 单写锁死锁。
	_, err := model.ConsumeAuthFlowWithAction(code, model.AuthFlowMatch{Purpose: model.AuthFlowPurposeWischoicerSSO}, func(tx *gorm.DB, f2 *model.AuthFlow) error {
		userID = f2.UserId
		if bsid == "" {
			return nil // cookie 缺失——F2 烧掉、不建 session
		}
		computed := wischoicerSsoBsidHash(bsid)
		if subtle.ConstantTimeCompare([]byte(computed), []byte(f2.Payload)) == 1 {
			matched = true
		}
		return nil // hash 不符或匹配——F2 都已烧；matched 仅匹配时置 true
	})

	setAuthNoStore(c)
	if err != nil || !matched {
		// 统一失败（cookie 缺失 / 格式错 / hash 不符 / F1 已消费）——bsid 类失败已提交 F2 烧码。不泄原因。
		c.Redirect(http.StatusFound, "/login")
		return
	}
	// session 创建在 consume tx 外（避免嵌套写 tx 死锁）。系统错→F2 已烧、统一失败。
	bundle, err := service.CreateLoginSession(userID, "wischoicer_sso", c.ClientIP(), c.Request.UserAgent())
	if err != nil {
		c.Redirect(http.StatusFound, "/login")
		return
	}
	service.WriteRefreshCookie(c, bundle.RefreshToken)
	model.UpdateUserLastLoginAt(userID)
	c.Redirect(http.StatusFound, "/dashboard")
}

// clearWischoicerSsoStateCookie 清除 state cookie（与 /start 的 set 对称：同 Path、Secure、HttpOnly、
// SameSite=Lax）。callback 无论成败都清（one-shot）。
func clearWischoicerSsoStateCookie(c *gin.Context) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(wischoicerSsoStateCookie, "", -1, wischoicerSsoStateCookiePath, "", true, true)
}

// generateWischoicerSsoBsid 生成 wischoicerSsoBsidBytes-byte 高熵 browser session id（base64url）。
func generateWischoicerSsoBsid() (string, error) {
	b := make([]byte, wischoicerSsoBsidBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// wischoicerSsoBsidHash 算 bsid 的 context-binding HMAC（key 派生自 SessionSecret，与 auth_flow
// token hash 用不同的 domain-separation 前缀）。存 F1.payload（C2 原样带入 F2.payload）；callback
// 恒时校验 cookie(bsid) 经同式 HMAC 后 == F2.payload。
func wischoicerSsoBsidHash(bsid string) string {
	return common.GenerateHMACWithKey([]byte("wischoicer-sso-bsid:"+common.SessionSecret), bsid)
}
