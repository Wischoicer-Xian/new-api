package vertex

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/QuantumNous/new-api/common"
	taskdto "github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

// TestDoResponseMasksModelForHiddenToken is the adaptor-level behavior
// regression for desensitization point 6 (the task submit-success response):
// the OpenAIVideo JSON returned by vertex.DoResponse carries the masked alias
// when the token is Hidden, and the real model name otherwise. It pins the
// 8th adaptor (the one missed in an earlier round), and asserts
// info.OriginModelName is never mutated.
//
// This PR's vertex path masks via info.MaskedModelName(), which reads the
// TokenHidden flag injected on the gin context (mirroring TokenUnlimited) —
// no token DB lookup on the response path — so the test sets TokenHidden
// directly and needs no database.
func TestDoResponseMasksModelForHiddenToken(t *testing.T) {
	const realModel = "veo-3.0-generate-001"
	const alias = common.MaskedSystemModelAlias

	// doResponse drives vertex.DoResponse against a fake upstream submit
	// response and returns the model field serialized into the client JSON.
	doResponse := func(t *testing.T, tokenHidden bool) (string, *taskdto.TaskError) {
		info := &relaycommon.RelayInfo{
			OriginModelName: realModel,
			TokenHidden:     tokenHidden,
		}
		info.ChannelMeta = &relaycommon.ChannelMeta{}
		// PublicTaskID is promoted from the embedded *TaskRelayInfo.
		info.TaskRelayInfo = &relaycommon.TaskRelayInfo{PublicTaskID: "task_vertex_test"}

		resp := &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(
				`{"name":"operations/projects/p/locations/us-central1/operations/op-1"}`,
			)),
		}

		gin.SetMode(gin.TestMode)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

		adaptor := &TaskAdaptor{}
		_, _, taskErr := adaptor.DoResponse(c, resp, info)

		// Red line: DoResponse only reads OriginModelName via MaskedModelName();
		// it must not mutate it.
		require.Equal(t, realModel, info.OriginModelName)

		var ov dto.OpenAIVideo
		require.NoError(t, common.Unmarshal(w.Body.Bytes(), &ov))
		return ov.Model, taskErr
	}

	t.Run("hidden token submit-success response masks the model", func(t *testing.T) {
		modelName, taskErr := doResponse(t, true)
		require.Nil(t, taskErr)
		require.Equal(t, alias, modelName)
	})

	t.Run("non-hidden token keeps the real model", func(t *testing.T) {
		modelName, taskErr := doResponse(t, false)
		require.Nil(t, taskErr)
		require.Equal(t, realModel, modelName)
	})
}
