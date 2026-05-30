package controller

import (
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
)

// SsoLogin accepts an access_token query parameter, validates it,
// creates a session for the user, and redirects to the specified URL.
// This enables seamless SSO from Wischoicer workstation to new-api.
func SsoLogin(c *gin.Context) {
	accessToken := c.Query("access_token")
	if accessToken == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "access_token is required",
		})
		return
	}

	redirectURL := c.Query("redirect")
	if redirectURL == "" {
		redirectURL = "/"
	}

	user, err := model.ValidateAccessToken(accessToken)
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
	c.Redirect(http.StatusFound, redirectURL)
}
