package controller

import (
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
)

type SsoLoginRequest struct {
	AccessToken string `json:"access_token" form:"access_token" binding:"required"`
	Redirect    string `json:"redirect" form:"redirect"`
}

// SsoLogin accepts access_token via POST JSON body or form data,
// validates it, creates a session, and redirects to a same-origin path.
// This enables seamless SSO from Wischoicer workstation to new-api.
// Supports both JSON (for fetch/XHR) and form POST (for cross-origin <form> submit).
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

	// Validate redirect: only allow relative paths starting with '/'
	// Block absolute URLs, protocol-relative URLs, and external domains
	redirectPath := "/"
	if req.Redirect != "" {
		cleaned := strings.TrimSpace(req.Redirect)
		if strings.HasPrefix(cleaned, "/") && !strings.HasPrefix(cleaned, "//") {
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

	// Create session
	session := sessions.Default(c)
	session.Set("id", user.Id)
	session.Set("username", user.Username)
	session.Set("role", user.Role)
	session.Set("status", user.Status)
	session.Set("group", user.Group)
	if err := session.Save(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "failed to save session",
		})
		return
	}

	model.UpdateUserLastLoginAt(user.Id)
	c.Redirect(http.StatusFound, redirectPath)
}
