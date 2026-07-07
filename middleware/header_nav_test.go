package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func withHeaderNavModules(t *testing.T, raw string) {
	t.Helper()

	common.OptionMapRWMutex.Lock()
	if common.OptionMap == nil {
		common.OptionMap = map[string]string{}
	}
	previous, hadPrevious := common.OptionMap["HeaderNavModules"]
	common.OptionMap["HeaderNavModules"] = raw
	common.OptionMapRWMutex.Unlock()

	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		defer common.OptionMapRWMutex.Unlock()
		if hadPrevious {
			common.OptionMap["HeaderNavModules"] = previous
			return
		}
		delete(common.OptionMap, "HeaderNavModules")
	})
}

func performHeaderNavRequest(t *testing.T, handler gin.HandlerFunc, authenticated bool) *httptest.ResponseRecorder {
	t.Helper()

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(sessions.Sessions("session", cookie.NewStore([]byte("header-nav-test"))))
	router.GET("/login", func(c *gin.Context) {
		session := sessions.Default(c)
		session.Set("username", "tester")
		session.Set("role", common.RoleCommonUser)
		session.Set("id", 1)
		session.Set("status", common.UserStatusEnabled)
		session.Set("group", "default")
		if err := session.Save(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false})
			return
		}
		c.Status(http.StatusNoContent)
	})
	router.GET("/api/test", handler, func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"success": true})
	})

	var cookies []*http.Cookie
	if authenticated {
		loginRecorder := httptest.NewRecorder()
		loginRequest := httptest.NewRequest(http.MethodGet, "/login", nil)
		router.ServeHTTP(loginRecorder, loginRequest)
		require.Equal(t, http.StatusNoContent, loginRecorder.Code)
		cookies = loginRecorder.Result().Cookies()
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	if authenticated {
		request.Header.Set("New-Api-User", "1")
		for _, cookie := range cookies {
			request.AddCookie(cookie)
		}
	}
	router.ServeHTTP(recorder, request)
	return recorder
}

// TestHeaderNavModuleAuthRequiresLoginForPricingByDefault locks the R3 decision 3
// contract change: with an empty HeaderNavModules config, the "pricing" module
// (which gates /api/perf-metrics/* and can reveal which model a hidden system
// key calls) defaults to RequireAuth=true, so an anonymous request is rejected.
// Previously this asserted AllowDefaultPublicAccess(200); it moved to 401 to
// track the pricing fallback change, not to force a green build.
func TestHeaderNavModuleAuthRequiresLoginForPricingByDefault(t *testing.T) {
	withHeaderNavModules(t, "")

	recorder := performHeaderNavRequest(t, HeaderNavModuleAuth("pricing"), false)

	require.Equal(t, http.StatusUnauthorized, recorder.Code)
}

func TestHeaderNavModuleAuthRejectsDisabledPricing(t *testing.T) {
	raw := `{"pricing":{"enabled":false,"requireAuth":false}}`
	withHeaderNavModules(t, raw)

	recorder := performHeaderNavRequest(t, HeaderNavModuleAuth("pricing"), false)

	require.Equal(t, http.StatusForbidden, recorder.Code)
}

func TestHeaderNavModuleAuthRequiresLoginForPricing(t *testing.T) {
	raw := `{"pricing":{"enabled":true,"requireAuth":true}}`
	withHeaderNavModules(t, raw)

	recorder := performHeaderNavRequest(t, HeaderNavModuleAuth("pricing"), false)

	require.Equal(t, http.StatusUnauthorized, recorder.Code)
}

func TestHeaderNavModuleAuthRequiresLoginForRankings(t *testing.T) {
	raw := `{"rankings":{"enabled":true,"requireAuth":true}}`
	withHeaderNavModules(t, raw)

	recorder := performHeaderNavRequest(t, HeaderNavModuleAuth("rankings"), false)

	require.Equal(t, http.StatusUnauthorized, recorder.Code)
}

func TestHeaderNavModuleAuthRejectsLegacyDisabledModule(t *testing.T) {
	raw := `{"rankings":false}`
	withHeaderNavModules(t, raw)

	recorder := performHeaderNavRequest(t, HeaderNavModuleAuth("rankings"), false)

	require.Equal(t, http.StatusForbidden, recorder.Code)
}

// TestHeaderNavModulePublicOrUserAuthRequiresLoginForPricingByDefault mirrors the
// Auth variant: an empty HeaderNavModules config now leaves "pricing" defaulting
// to RequireAuth=true (R3 decision 3), so the public-or-user-auth gate rejects an
// anonymous request. Previously AllowDefaultPublicAccess(200); moved to 401 to
// track the pricing fallback change, not to force green.
func TestHeaderNavModulePublicOrUserAuthRequiresLoginForPricingByDefault(t *testing.T) {
	withHeaderNavModules(t, "")

	recorder := performHeaderNavRequest(t, HeaderNavModulePublicOrUserAuth("pricing"), false)

	require.Equal(t, http.StatusUnauthorized, recorder.Code)
}

func TestHeaderNavModulePublicOrUserAuthRequiresLoginWhenDisabled(t *testing.T) {
	raw := `{"pricing":{"enabled":false,"requireAuth":false}}`
	withHeaderNavModules(t, raw)

	recorder := performHeaderNavRequest(t, HeaderNavModulePublicOrUserAuth("pricing"), false)

	require.Equal(t, http.StatusUnauthorized, recorder.Code)
}

func TestHeaderNavModulePublicOrUserAuthAllowsLoggedInWhenDisabled(t *testing.T) {
	raw := `{"pricing":{"enabled":false,"requireAuth":false}}`
	withHeaderNavModules(t, raw)

	recorder := performHeaderNavRequest(t, HeaderNavModulePublicOrUserAuth("pricing"), true)

	require.Equal(t, http.StatusOK, recorder.Code)
}

func TestHeaderNavModulePublicOrUserAuthRequiresLoginWhenRequireAuth(t *testing.T) {
	raw := `{"pricing":{"enabled":true,"requireAuth":true}}`
	withHeaderNavModules(t, raw)

	recorder := performHeaderNavRequest(t, HeaderNavModulePublicOrUserAuth("pricing"), false)

	require.Equal(t, http.StatusUnauthorized, recorder.Code)
}

func TestHeaderNavModulePublicOrUserAuthRequiresLoginForLegacyDisabledModule(t *testing.T) {
	raw := `{"pricing":false}`
	withHeaderNavModules(t, raw)

	recorder := performHeaderNavRequest(t, HeaderNavModulePublicOrUserAuth("pricing"), false)

	require.Equal(t, http.StatusUnauthorized, recorder.Code)
}
