package controller

import (
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
)

type SsoLoginRequest struct {
	AccessToken string `json:"access_token" form:"access_token" binding:"required"`
	Redirect    string `json:"redirect" form:"redirect"`
}

// SsoLogin accepts a new-api personal access_token (PAT) via POST JSON body or
// form data — the PAT that wischoicer-user mints via the admin
// /api/user/{id}/generate_access_token endpoint — validates it, and establishes
// a real browser login session on new-api.
//
// It reuses the same dashboard login flow as password login: a user_sessions
// row is created and the httpOnly refresh cookie is set, then the request is
// redirected to a same-origin path. The new-api SPA exchanges the refresh
// cookie for a short-lived Authorization access_token on bootstrap
// (web/src/lib/auth-session.ts:bootstrapAuthentication -> POST /api/user/auth/refresh),
// so subsequent UserAuth-protected routes (including /api/wallet/*) authenticate.
//
// This replaces an earlier gin-contrib/sessions cookie implementation which
// (a) panicked because the sessions middleware was never registered anywhere
// in the app, and (b) could never authenticate dashboard/wallet routes, since
// middleware.UserAuth reads the Authorization Bearer header (dashboard JWT or
// PAT), not a gin-contrib/sessions cookie.
//
// Supports both JSON (for fetch/XHR) and form POST (for cross-origin <form>
// submit, which is how wischoicer workstation triggers SSO).
func SsoLogin(c *gin.Context) {
	var req SsoLoginRequest
	contentType := c.GetHeader("Content-Type")
	if strings.HasPrefix(contentType, "application/json") {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"success": false,
				"message": "access_token is required",
			})
			return
		}
	} else {
		// Form POST (application/x-www-form-urlencoded or multipart)
		req.AccessToken = c.PostForm("access_token")
		req.Redirect = c.PostForm("redirect")
		if req.AccessToken == "" {
			c.JSON(http.StatusBadRequest, gin.H{
				"success": false,
				"message": "access_token is required",
			})
			return
		}
	}

	// Confine the redirect to a same-origin path. See safeSameOriginRedirect:
	// it rejects ASCII control bytes and backslashes anywhere (browsers strip
	// TAB/CR/LF and normalize /\ during URL parsing, which otherwise turns
	// "/\t/host" or "/\host" into the authority "//host"), then requires a
	// single leading '/' and no scheme/host.
	redirectPath := safeSameOriginRedirect(req.Redirect)

	user, err := model.ValidateAccessToken(req.AccessToken)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"message": "invalid access_token",
		})
		return
	}
	if user == nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"message": "user not found for given access_token",
		})
		return
	}
	if user.Status != common.UserStatusEnabled {
		c.JSON(http.StatusForbidden, gin.H{
			"success": false,
			"message": "user is disabled",
		})
		return
	}

	// Issue a real dashboard login session (same path as password login) and
	// set the httpOnly refresh cookie. CreateLoginSession enforces the active/
	// issuance session limits and the user-status/auth-version invariants;
	// writeAuthSessionError maps those onto the standard AUTH_* codes so a
	// session-limit or transient failure is reported consistently.
	bundle, err := service.CreateLoginSession(user.Id, "sso", c.ClientIP(), c.Request.UserAgent())
	if err != nil {
		writeAuthSessionError(c, err)
		return
	}
	service.WriteRefreshCookie(c, bundle.RefreshToken)
	model.UpdateUserLastLoginAt(user.Id)
	// Match the password-login response: never cache a session-bearing reply.
	setAuthNoStore(c)
	c.Redirect(http.StatusFound, redirectPath)
}

// safeSameOriginRedirect validates and normalizes a user-supplied redirect
// target for use as a 302 Location, confining it to a same-origin path.
//
// It rejects anything a browser could reinterpret as a cross-origin URL:
//   - ASCII control bytes (< 0x20 or 0x7f) anywhere — browsers strip TAB/CR/LF
//     during URL parsing, so e.g. "/\t/host" resolves to the authority "//host".
//   - backslashes anywhere — several browsers normalize "/\host" to "//host".
//   - any URL with a scheme or host (absolute "https://host" or scheme-relative
//     "//host").
//
// Only a path beginning with a single '/' is accepted; everything else (empty,
// relative, authority, control/backslash smuggling, or percent-decoded bypasses
// like "%2F%09%2Fhost" — the form parser decodes that to "/\t/host") falls back
// to "/". The accepted path is collapsed with path.Clean; a query/fragment is
// preserved.
func safeSameOriginRedirect(raw string) string {
	cleaned := strings.TrimSpace(raw)
	if cleaned == "" || cleaned[0] != '/' {
		return "/"
	}
	for i := 0; i < len(cleaned); i++ {
		b := cleaned[i]
		if b == '\\' || b < 0x20 || b == 0x7f {
			return "/"
		}
	}
	parsed, err := url.Parse(cleaned)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" {
		return "/"
	}
	normalized := path.Clean(parsed.Path)
	if normalized == "." || normalized == "" {
		return "/"
	}
	if parsed.RawQuery != "" {
		normalized += "?" + parsed.RawQuery
	}
	if parsed.Fragment != "" {
		normalized += "#" + parsed.Fragment
	}
	return normalized
}
