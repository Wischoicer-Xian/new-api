package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestImageTaskRouterTreeAcceptsStaticAndParamSiblings locks in the §6.1 route
// shape: the image-task group registers POST /:task_id/cancel alongside POST
// /generations and POST /edits, which mixes a param segment with static
// siblings under the same prefix and method. gin's tree must accept that
// (static wins over param), and the param two-segment route must still match —
// otherwise the server would panic at boot or misroute create traffic onto the
// cancel handler. This test runs against a bare engine so it isolates the tree
// registration from the full middleware stack.
func TestImageTaskRouterTreeAcceptsStaticAndParamSiblings(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/v1/image-tasks")
	g.GET("/:task_id", func(c *gin.Context) { c.String(http.StatusOK, "get:%s", c.Param("task_id")) })
	g.POST("/:task_id/cancel", func(c *gin.Context) { c.String(http.StatusOK, "cancel:%s", c.Param("task_id")) })
	g.POST("/generations", func(c *gin.Context) { c.String(http.StatusOK, "generations") })
	g.POST("/edits", func(c *gin.Context) { c.String(http.StatusOK, "edit") })

	cases := []struct {
		method string
		path   string
		want   string
	}{
		{http.MethodPost, "/v1/image-tasks/generations", "generations"},
		{http.MethodPost, "/v1/image-tasks/edits", "edit"},
		{http.MethodPost, "/v1/image-tasks/imgtask_abc/cancel", "cancel:imgtask_abc"},
		{http.MethodGet, "/v1/image-tasks/imgtask_abc", "get:imgtask_abc"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		require.Equalf(t, http.StatusOK, w.Code, "route %s %s", tc.method, tc.path)
		require.Equalf(t, tc.want, w.Body.String(), "body for %s %s", tc.method, tc.path)
	}
}
