package controller

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImageCapabilityPreview(t *testing.T) {
	gin.SetMode(gin.TestMode)

	doPost := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
		c.Request.Header.Set("Content-Type", "application/json")
		ImageCapabilityPreview(c)
		return w
	}

	parse := func(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var m map[string]any
		require.NoError(t, common.Unmarshal(w.Body.Bytes(), &m))
		return m
	}

	t.Run("openai capable resolves defaults", func(t *testing.T) {
		w := doPost(`{"type":1,"image_execution_config":"{\"defaults\":{\"generation\":\"sync\",\"edit\":\"sync\"}}"}`)
		m := parse(t, w)
		require.True(t, m["success"].(bool))
		data := m["data"].(map[string]any)
		assert.True(t, data["image_capable"].(bool))
		assert.Equal(t, "openai-image-adapter/v1", data["adapter_version"])
		sup := data["support"].(map[string]any)
		assert.Equal(t, []any{"sync"}, sup["generation"])
		assert.Equal(t, []any{"sync"}, sup["edit"])
		preview := data["preview"].([]any)
		assert.Len(t, preview, 2)
	})

	t.Run("non image-capable type reports not capable", func(t *testing.T) {
		w := doPost(`{"type":14,"image_execution_config":"{\"defaults\":{\"generation\":\"sync\"}}"}`)
		m := parse(t, w)
		require.True(t, m["success"].(bool))
		data := m["data"].(map[string]any)
		assert.False(t, data["image_capable"].(bool))
	})

	t.Run("unknown type rejected not degraded to openai", func(t *testing.T) {
		// P1-2: ChannelType2APIType mapping bool is honored; an unknown type is
		// not silently treated as OpenAI sync.
		w := doPost(`{"type":99999,"image_execution_config":"{\"defaults\":{\"generation\":\"sync\"}}"}`)
		m := parse(t, w)
		assert.False(t, m["success"].(bool))
		require.Contains(t, m["message"].(string), "未知渠道类型")
	})

	t.Run("invalid config returns success false", func(t *testing.T) {
		w := doPost(`{"type":1,"image_execution_config":"{\"defaults\": "}`)
		m := parse(t, w)
		assert.False(t, m["success"].(bool))
		require.Contains(t, m["message"].(string), "格式错误")
	})

	t.Run("model override reported alongside defaults", func(t *testing.T) {
		w := doPost(`{"type":1,"image_execution_config":"{\"models\":{\"gpt-image-1\":{\"edit\":\"sync\"}}}"}`)
		m := parse(t, w)
		require.True(t, m["success"].(bool))
		data := m["data"].(map[string]any)
		preview := data["preview"].([]any)
		// Two channel-wide default entries plus one model override entry.
		assert.Len(t, preview, 3)
	})

	t.Run("empty config on capable type still resolves adapter default", func(t *testing.T) {
		w := doPost(`{"type":1,"image_execution_config":""}`)
		m := parse(t, w)
		require.True(t, m["success"].(bool))
		data := m["data"].(map[string]any)
		assert.True(t, data["image_capable"].(bool))
	})
}
