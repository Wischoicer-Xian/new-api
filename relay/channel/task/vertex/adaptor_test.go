package vertex

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// TestMain gives the vertex adaptor tests an in-memory SQLite database so
// model.MaskedModelName(tokenId, ...) — which resolves the token's Hidden flag
// via model.GetTokenById — works without a real DB. Mirrors the model package
// test setup, scoped to what the desensitization regression needs.
func TestMain(m *testing.M) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		panic("failed to open test db: " + err.Error())
	}
	model.DB = db
	common.RedisEnabled = false
	if err := db.AutoMigrate(
		&model.Task{},
		&model.User{},
		&model.Token{},
	); err != nil {
		panic("failed to migrate: " + err.Error())
	}
	os.Exit(m.Run())
}

// TestDoResponseMasksModelForHiddenToken is the adaptor-level behavior
// regression for desensitization point 6 (the task submit-success response):
// the OpenAIVideo JSON returned by vertex.DoResponse carries the masked alias
// when the token is Hidden, and the real model name otherwise. This pins the
// exact face missed in the prior round (vertex was the 8th adaptor left
// un-masked), and asserts info.OriginModelName is never mutated.
func TestDoResponseMasksModelForHiddenToken(t *testing.T) {
	require.NoError(t, model.DB.Create(&model.Token{Id: 7001, UserId: 1, Key: "sk-hidden", Name: "知言云策系统账号", Hidden: true}).Error)
	require.NoError(t, model.DB.Create(&model.Token{Id: 7002, UserId: 1, Key: "sk-normal", Name: "normal", Hidden: false}).Error)

	const realModel = "veo-3.0-generate-001"
	const alias = "知言云策系统调用"

	// doResponse drives vertex.DoResponse against a fake upstream submit
	// response and returns the model field serialized into the client JSON.
	doResponse := func(t *testing.T, tokenId int) (string, *dto.TaskError) {
		info := &relaycommon.RelayInfo{
			TokenId:         tokenId,
			OriginModelName: realModel,
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

		// Red line: DoResponse only reads OriginModelName; it must not mutate it.
		require.Equal(t, realModel, info.OriginModelName)

		var ov dto.OpenAIVideo
		require.NoError(t, common.Unmarshal(w.Body.Bytes(), &ov))
		return ov.Model, taskErr
	}

	t.Run("hidden token submit-success response masks the model", func(t *testing.T) {
		modelName, taskErr := doResponse(t, 7001)
		require.Nil(t, taskErr)
		require.Equal(t, alias, modelName)
	})

	t.Run("non-hidden token keeps the real model", func(t *testing.T) {
		modelName, taskErr := doResponse(t, 7002)
		require.Nil(t, taskErr)
		require.Equal(t, realModel, modelName)
	})
}
