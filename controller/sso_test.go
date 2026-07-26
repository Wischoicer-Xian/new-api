package controller

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// setupSsoTestDB stands up an in-memory SQLite database with the tables SsoLogin
// touches, creates one enabled user with a known PAT, and restores global state
// on cleanup. Mirrors the fixture style in auth_session_test.go.
func setupSsoTestDB(t *testing.T) (*gorm.DB, *model.User, string) {
	t.Helper()
	previousDB := model.DB
	previousRedis := common.RedisEnabled
	previousSecret := common.SessionSecret
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.UserSession{}))
	model.DB = db
	common.RedisEnabled = false
	common.SessionSecret = "sso-login-test-secret"
	t.Cleanup(func() {
		model.DB = previousDB
		common.RedisEnabled = previousRedis
		common.SessionSecret = previousSecret
	})

	pat := "sso-test-pat-0123456789abcdef01234567"
	user := &model.User{
		Username: "sso-user", Password: "unused", Role: common.RoleCommonUser,
		Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1,
	}
	user.SetAccessToken(pat)
	require.NoError(t, db.Create(user).Error)
	return db, user, pat
}

// callSsoLogin drives SsoLogin through a real gin engine via ServeHTTP so the
// full response lifecycle runs (headers flush, redirect status, cookies). A
// direct gin.CreateTestContext call would not flush the 302 status to the
// recorder, because c.Redirect writes no body and gin defers the header flush.
func callSsoLogin(pat, redirect string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/api/sso/login", SsoLogin)

	form := url.Values{}
	if pat != "" {
		form.Set("access_token", pat)
	}
	if redirect != "" {
		form.Set("redirect", redirect)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/sso/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "127.0.0.1:1234"

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)
	return recorder
}

func findRefreshCookie(recorder *httptest.ResponseRecorder) *http.Cookie {
	for _, ck := range recorder.Result().Cookies() {
		if ck.Name == service.RefreshCookieName {
			return ck
		}
	}
	return nil
}

func countUserSessions(t *testing.T, userID int) int64 {
	t.Helper()
	var n int64
	require.NoError(t, model.DB.Model(&model.UserSession{}).Where("user_id = ?", userID).Count(&n).Error)
	return n
}

// TestSsoLogin_MintsSessionAndRedirects is the core regression: a valid PAT no
// longer panics (the gin-contrib/sessions path did), and instead issues a real
// dashboard login session plus the httpOnly refresh cookie the SPA boots from.
func TestSsoLogin_MintsSessionAndRedirects(t *testing.T) {
	_, user, pat := setupSsoTestDB(t)

	recorder := callSsoLogin(pat, "/")

	require.Equal(t, http.StatusFound, recorder.Code, "expected 302 redirect, got body: %s", recorder.Body.String())
	assert.Equal(t, "/", recorder.Header().Get("Location"))
	assert.Equal(t, "no-store", recorder.Header().Get("Cache-Control"), "session-bearing 302 must not be cached")
	cookie := findRefreshCookie(recorder)
	require.NotNil(t, cookie, "refresh cookie must be set")
	assert.True(t, cookie.HttpOnly)
	assert.Equal(t, "/api/user/auth", cookie.Path)
	assert.Equal(t, http.SameSiteStrictMode, cookie.SameSite)
	assert.Equal(t, int64(1), countUserSessions(t, user.Id))
}

// TestSsoLogin_InvalidAccessToken returns 401 and writes no session/cookie,
// confirming PAT validation still gates the flow (the path that used to 401).
func TestSsoLogin_InvalidAccessToken(t *testing.T) {
	_, user, _ := setupSsoTestDB(t)

	recorder := callSsoLogin("not-a-real-pat", "/")

	assert.Equal(t, http.StatusUnauthorized, recorder.Code)
	assert.Nil(t, findRefreshCookie(recorder))
	assert.Equal(t, int64(0), countUserSessions(t, user.Id))
}

// TestSsoLogin_BlocksOpenRedirect locks the redirect sanitizer: the 302
// Location is confined to same-origin relative paths, never an attacker URL.
// Vectors follow the R3 review: control-byte smuggling (browsers strip
// TAB/CR/LF → authority), backslash normalization, repeated leading slashes,
// absolute/scheme-relative URLs, and non-path schemes.
func TestSsoLogin_BlocksOpenRedirect(t *testing.T) {
	_, _, pat := setupSsoTestDB(t)

	tests := []struct {
		name     string
		redirect string
		want     string
	}{
		{"empty_defaults_root", "", "/"},
		{"same_origin_path_allowed", "/console", "/console"},
		{"wallet_path_allowed", "/wallet", "/wallet"},
		{"query_preserved", "/console?tab=billing", "/console?tab=billing"},
		{"fragment_preserved", "/console#billing", "/console#billing"},
		{"dot_segment_normalized", "/a/../b", "/b"},
		{"protocol_relative_authority", "//evil.example.com", "/"},
		// "///host" is not an authority — browsers and net/url treat the empty
		// authority as same-origin, so it collapses to the path "/host" (safe).
		{"triple_leading_slash_collapses_to_path", "///evil.example.com", "/evil.example.com"},
		{"absolute_https_url", "https://evil.example.com/", "/"},
		{"backslash_authority", "/\\evil.example.com", "/"},
		{"htab_smuggling", "/\t/evil.example.com", "/"},
		{"cr_smuggling", "/\r/evil.example.com", "/"},
		{"lf_smuggling", "/\n/evil.example.com", "/"},
		{"null_byte", "/\x00evil", "/"},
		{"interior_control_byte", "/foo\tbar", "/"},
		{"relative_no_leading_slash", "relative/path", "/"},
		{"javascript_scheme", "javascript:alert(1)", "/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := callSsoLogin(pat, tt.redirect)
			require.Equal(t, http.StatusFound, recorder.Code, "body: %s", recorder.Body.String())
			assert.Equal(t, tt.want, recorder.Header().Get("Location"))
		})
	}
}

// TestSsoLogin_NeutralizesEncodedRedirectBypass exercises the exact R3 bypass
// through real form parsing: the form value "%2F%09%2Fevil.example" is decoded
// by the application/x-www-form-urlencoded parser into "/<TAB>/evil.example",
// which a browser then parses as the authority "//evil.example". Must land on "/".
func TestSsoLogin_NeutralizesEncodedRedirectBypass(t *testing.T) {
	_, _, pat := setupSsoTestDB(t)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/api/sso/login", SsoLogin)

	body := "access_token=" + pat + "&redirect=%2F%09%2Fevil.example"
	req := httptest.NewRequest(http.MethodPost, "/api/sso/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "127.0.0.1:1234"

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)
	require.Equal(t, http.StatusFound, recorder.Code)
	assert.Equal(t, "/", recorder.Header().Get("Location"), "encoded control-byte bypass must be neutralized")
}

// TestSsoLogin_RefreshCookieBootstrapsAccessViaUserAuth is the end-to-end closed
// loop the R3 review asked for: SSO mints a refresh cookie, that cookie is fed
// to /api/user/auth/refresh to obtain an access_token, and the access_token then
// authenticates against a real middleware.UserAuth-protected route — resolving
// back to the SSO user. This proves the SPA bootstrap chain actually works.
func TestSsoLogin_RefreshCookieBootstrapsAccessViaUserAuth(t *testing.T) {
	_, user, pat := setupSsoTestDB(t)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/api/sso/login", SsoLogin)
	engine.POST("/api/user/auth/refresh", RefreshAuth)
	engine.GET("/protected", middleware.UserAuth(), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"id": c.GetInt("id"), "role": c.GetInt("role")})
	})

	// 1) SSO login -> 302 + refresh cookie.
	form := url.Values{}
	form.Set("access_token", pat)
	ssoReq := httptest.NewRequest(http.MethodPost, "/api/sso/login", strings.NewReader(form.Encode()))
	ssoReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	ssoReq.RemoteAddr = "127.0.0.1:1234"
	ssoRecorder := httptest.NewRecorder()
	engine.ServeHTTP(ssoRecorder, ssoReq)
	require.Equal(t, http.StatusFound, ssoRecorder.Code, "sso body: %s", ssoRecorder.Body.String())
	refreshCookie := findRefreshCookie(ssoRecorder)
	require.NotNil(t, refreshCookie, "SSO must set the refresh cookie")

	// 2) refresh cookie -> access_token.
	refreshReq := httptest.NewRequest(http.MethodPost, "/api/user/auth/refresh", nil)
	refreshReq.AddCookie(refreshCookie)
	refreshReq.RemoteAddr = "127.0.0.1:1234"
	refreshRecorder := httptest.NewRecorder()
	engine.ServeHTTP(refreshRecorder, refreshReq)
	require.Equal(t, http.StatusOK, refreshRecorder.Code, "refresh body: %s", refreshRecorder.Body.String())
	var refreshBody struct {
		Success bool `json:"success"`
		Data    struct {
			AccessToken string `json:"access_token"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(refreshRecorder.Body.Bytes(), &refreshBody))
	require.True(t, refreshBody.Success)
	require.NotEmpty(t, refreshBody.Data.AccessToken, "refresh must mint an access_token")

	// 3) access_token -> middleware.UserAuth-protected route.
	protectedReq := httptest.NewRequest(http.MethodGet, "/protected", nil)
	protectedReq.Header.Set("Authorization", "Bearer "+refreshBody.Data.AccessToken)
	protectedRecorder := httptest.NewRecorder()
	engine.ServeHTTP(protectedRecorder, protectedReq)
	require.Equal(t, http.StatusOK, protectedRecorder.Code, "protected body: %s", protectedRecorder.Body.String())
	var protectedBody struct {
		ID   int `json:"id"`
		Role int `json:"role"`
	}
	require.NoError(t, common.Unmarshal(protectedRecorder.Body.Bytes(), &protectedBody))
	assert.Equal(t, user.Id, protectedBody.ID, "UserAuth must resolve to the SSO user")
	assert.Equal(t, user.Role, protectedBody.Role)
}
