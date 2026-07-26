package controller

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// setupWischoicerSsoStartTestDB 立一个 in-memory SQLite（含 auth_flows）+ 配一个合法 authorize
// URL（SsoStart 读 common.WischoicerSsoAuthorizeURL），cleanup 恢复。镜像 sso_test.go 的 fixture 风格。
func setupWischoicerSsoStartTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	prevDB := model.DB
	prevAuth := common.WischoicerSsoAuthorizeURL
	prevSecret := common.SessionSecret
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.AuthFlow{}))
	model.DB = db
	common.WischoicerSsoAuthorizeURL = "https://test.wischoicer.com/bff/gateway/user/sso/authorize"
	common.SessionSecret = "wischoicer-sso-start-test-secret"
	t.Cleanup(func() {
		model.DB = prevDB
		common.WischoicerSsoAuthorizeURL = prevAuth
		common.SessionSecret = prevSecret
	})
	return db
}

// RFC v4 §2 N1: /start 建 AuthFlow + 种 state cookie（值=flow_token，HttpOnly+Secure+
// SameSite=Lax+Path=/）+ 302 到 authorize URL?flow_token=F1（url builder，固定 path）。
func TestSsoStart_RedirectsWithFlowTokenAndStateCookie(t *testing.T) {
	setupWischoicerSsoStartTestDB(t)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	_, r := gin.CreateTestContext(w)
	r.GET("/api/sso/wischoicer/start", SsoStart)

	req := httptest.NewRequest(http.MethodGet, "/api/sso/wischoicer/start", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusFound, w.Code, "want 302, body=%s", w.Body.String())

	// 302 Location = authorize 固定 URL + ?flow_token=<非空>。
	loc := w.Header().Get("Location")
	parsed, err := url.Parse(loc)
	require.NoError(t, err)
	assert.Equal(t, "https", parsed.Scheme)
	assert.Equal(t, "test.wischoicer.com", parsed.Host)
	assert.Equal(t, "/bff/gateway/user/sso/authorize", parsed.Path)
	flowToken := parsed.Query().Get("flow_token")
	assert.NotEmpty(t, flowToken, "flow_token in redirect must be non-empty")

	// state cookie 种下、值 == redirect 的 flow_token（绑定发起浏览器）、属性齐。
	var cookie *http.Cookie
	for _, ck := range w.Result().Cookies() {
		if ck.Name == wischoicerSsoStateCookie {
			cookie = ck
		}
	}
	require.NotNil(t, cookie, "state cookie %q not set", wischoicerSsoStateCookie)
	assert.Equal(t, flowToken, cookie.Value, "state cookie value must equal flow_token")
	assert.True(t, cookie.HttpOnly, "state cookie must be HttpOnly")
	assert.True(t, cookie.Secure, "state cookie must be Secure (HTTPS-only)")
	assert.Equal(t, http.SameSiteLaxMode, cookie.SameSite, "state cookie must be SameSite=Lax")
	// RFC §4 line 178: Path 收窄到 /api/sso/wischoicer（/start 与 callback 同前缀，不向其它路径泄漏）。
	assert.Equal(t, wischoicerSsoStateCookiePath, cookie.Path, "state cookie Path must be narrowed to /api/sso/wischoicer")
	// RFC §4 line 178: no-store（不缓存含 flow_token 的 302）+ no-referrer（不让 Location 里的 flow_token 经 Referer 泄漏）。
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	assert.Equal(t, "no-referrer", w.Header().Get("Referrer-Policy"))
}
