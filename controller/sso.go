package controller

import (
	"net/http"
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

	// Validate redirect: only allow same-origin relative paths. Accept a single
	// leading '/' and reject anything that could be interpreted as an authority
	// — including protocol-relative '//host' and the backslash form '/\host',
	// which several browsers normalize to '//host'. This keeps the 302 Location
	// confined to a same-origin path and blocks open-redirect.
	redirectPath := "/"
	if req.Redirect != "" {
		cleaned := strings.TrimSpace(req.Redirect)
		if len(cleaned) >= 1 && cleaned[0] == '/' && !(len(cleaned) >= 2 && (cleaned[1] == '/' || cleaned[1] == '\\')) {
			redirectPath = cleaned
		}
	}

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
	c.Redirect(http.StatusFound, redirectPath)
}
