package doubao

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/common"
)

// TestConvertToRequestPayload_DurationPassthrough 锁定 doubao adapter duration 透传合同（WIS-462）：
// content-workstation 等只送 duration(int) 的 caller 也要把时长透传到 Seedance 上游，不被默认 5s。
// 镜像 gemini ResolveVeoDuration：Seconds（string）优先，空时回退 Duration（int）。
func TestConvertToRequestPayload_DurationPassthrough(t *testing.T) {
	cases := []struct {
		name     string
		seconds  string
		duration int
		wantDur  *int // nil = 不设上游 duration
		wantNote string
	}{
		{
			name:     "Seconds 空 + Duration>0 → 透传 Duration",
			seconds:  "",
			duration: 15,
			wantDur:  intPtr(15),
			wantNote: "content-workstation 送 duration=15（Seconds 空）必须透传到上游，不再被默认 5s",
		},
		{
			name:     "Seconds 优先于 Duration",
			seconds:  "10",
			duration: 15,
			wantDur:  intPtr(10),
			wantNote: "Seconds（string）与 Duration 并存时 Seconds 优先（gemini 同口径）",
		},
		{
			name:     "两者都空 → 不设上游 duration",
			seconds:  "",
			duration: 0,
			wantDur:  nil,
			wantNote: "都没给 → 不设 r.Duration（上游走自己的默认）",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &common.TaskSubmitReq{
				Model:    "doubao-seedance-2-0-260128",
				Seconds:  tc.seconds,
				Duration: tc.duration,
			}
			payload, err := (&TaskAdaptor{}).convertToRequestPayload(req)
			require.NoError(t, err, tc.wantNote)
			require.NotNil(t, payload, tc.wantNote)

			switch {
			case tc.wantDur == nil:
				assert.Nil(t, payload.Duration, "expected no upstream duration: %s", tc.wantNote)
			default:
				if assert.NotNil(t, payload.Duration, "expected upstream duration set: %s", tc.wantNote) {
					assert.Equal(t, *tc.wantDur, int(*payload.Duration), tc.wantNote)
				}
			}
		})
	}
}

func intPtr(v int) *int { return &v }

// TestConvertToOpenAIVideo_LastFrameURL 锁定 doubao 尾帧透出合同（video-clone 首尾帧衔接）：
// 上游 return_last_frame=true 时查询响应带 content.last_frame_url（官方文档已锁定字段名），
// 必须透出到 OpenAI Video metadata.last_frame_url 供调用方下载转存；上游未返回时 metadata 不含该键。
func TestConvertToOpenAIVideo_LastFrameURL(t *testing.T) {
	newTask := func(data string) *model.Task {
		return &model.Task{
			TaskID:     "cgt-test",
			Status:     model.TaskStatusSuccess,
			Progress:   "100%",
			CreatedAt:  1,
			UpdatedAt:  2,
			Data:       json.RawMessage(data),
			Properties: model.Properties{OriginModelName: "doubao-seedance-2-0-260128"},
		}
	}

	t.Run("有尾帧则透出", func(t *testing.T) {
		task := newTask(`{"id":"cgt-test","status":"succeeded","content":{"video_url":"https://x/v.mp4","last_frame_url":"https://x/last.png"}}`)
		out, err := (&TaskAdaptor{}).ConvertToOpenAIVideo(task)
		require.NoError(t, err)
		var ov map[string]any
		require.NoError(t, json.Unmarshal(out, &ov))
		meta, ok := ov["metadata"].(map[string]any)
		require.True(t, ok, "metadata 必须存在（url 恒透出有值）")
		assert.Equal(t, "https://x/last.png", meta["last_frame_url"])
		assert.Equal(t, "https://x/v.mp4", meta["url"])
	})

	t.Run("无尾帧不透出", func(t *testing.T) {
		task := newTask(`{"id":"cgt-test","status":"succeeded","content":{"video_url":"https://x/v.mp4"}}`)
		out, err := (&TaskAdaptor{}).ConvertToOpenAIVideo(task)
		require.NoError(t, err)
		var ov map[string]any
		require.NoError(t, json.Unmarshal(out, &ov))
		meta, _ := ov["metadata"].(map[string]any)
		_, has := meta["last_frame_url"]
		assert.False(t, has, "上游未返回尾帧时不得出现 last_frame_url 键")
	})
}
