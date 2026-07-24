package controller

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
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
func TestSsoLogin_BlocksOpenRedirect(t *testing.T) {
	_, _, pat := setupSsoTestDB(t)

	tests := []struct {
		name        string
		redirect    string
		want        string
		description string
	}{
		{"empty_redirect_defaults_root", "", "/", "empty redirect falls back to root"},
		{"same_origin_path_allowed", "/console", "/console", "same-origin relative path is honored"},
		{"wallet_path_allowed", "/wallet", "/wallet", "same-origin wallet path (the SSO target) is honored"},
		{"protocol_relative_blocked", "//evil.example.com", "/", "protocol-relative URL is neutralized"},
		{"absolute_https_url_blocked", "https://evil.example.com/", "/", "absolute URL is neutralized"},
		{"backslash_authority_blocked", "/\\evil.example.com", "/", "backslash form (browsers normalize to //host) is neutralized"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := callSsoLogin(pat, tt.redirect)
			require.Equal(t, http.StatusFound, recorder.Code)
			assert.Equal(t, tt.want, recorder.Header().Get("Location"), tt.description)
		})
	}
}
