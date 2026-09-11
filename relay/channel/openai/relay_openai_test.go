package openai

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// buildAudioSSE constructs an SSE stream body that mimics OpenAI audio model
// streaming behavior: usage info appears in the second-to-last data chunk.
//
// Layout: chunk1 (content) -> chunk2 (content + usage) -> chunk3 (finish) -> [DONE]
// After processing, secondLastStreamData = chunk2 (carries usage).
func buildAudioSSE(upstreamModel string, usage dto.Usage) []byte {
	var b bytes.Buffer

	// Chunk 1: content delta
	b.WriteString(fmt.Sprintf(`data: {"id":"chatcmpl-test","object":"chat.completion.chunk","model":"%s","choices":[{"index":0,"delta":{"content":"Hello"}}]}`, upstreamModel))
	b.WriteString("\n\n")

	// Chunk 2: content + usage (second-to-last -> becomes secondLastStreamData)
	b.WriteString(fmt.Sprintf(`data: {"id":"chatcmpl-test","object":"chat.completion.chunk","model":"%s","choices":[{"index":0,"delta":{"content":" world"}}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
		upstreamModel, usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens))
	b.WriteString("\n\n")

	// Chunk 3: finish (last -> becomes lastStreamData, no usage)
	b.WriteString(fmt.Sprintf(`data: {"id":"chatcmpl-test","object":"chat.completion.chunk","model":"%s","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, upstreamModel))
	b.WriteString("\n\n")

	b.WriteString("data: [DONE]\n\n")
	return b.Bytes()
}

type nopCloser struct{ io.Reader }

func (nopCloser) Close() error { return nil }

// TestOaiStreamHandler_AudioModelDetection_MappedModel verifies that when the
// caller model name does not contain "audio" but the upstream model is an audio
// model (model_mapping scenario), the handler still extracts usage from the
// second-to-last SSE chunk instead of falling back to text-based estimation.
func TestOaiStreamHandler_AudioModelDetection_MappedModel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	oldStreamingTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 300
	t.Cleanup(func() { constant.StreamingTimeout = oldStreamingTimeout })

	callerModel := "my-voice-bot"
	upstreamModel := "gpt-4o-audio-preview"

	upstreamUsage := dto.Usage{
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
	}

	sseBody := buildAudioSSE(upstreamModel, upstreamUsage)
	resp := &http.Response{
		Body:   nopCloser{strings.NewReader(string(sseBody))},
		Header: make(http.Header),
	}
	resp.Header.Set("Content-Type", "text/event-stream")

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	info := &relaycommon.RelayInfo{
		RelayFormat:     types.RelayFormatOpenAI,
		OriginModelName: callerModel,
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: upstreamModel,
		},
		RelayMode: relayconstant.RelayModeChatCompletions,
	}
	info.SetEstimatePromptTokens(10)

	usage, apiErr := OaiStreamHandler(c, info, resp)
	require.Nil(t, apiErr, "OaiStreamHandler should not return an error")
	require.NotNil(t, usage, "usage should not be nil")

	require.Equal(t, 100, usage.PromptTokens,
		"PromptTokens should match upstream audio usage (not estimated)")
	require.Equal(t, 50, usage.CompletionTokens,
		"CompletionTokens should match upstream audio usage (not estimated)")
	require.Equal(t, 150, usage.TotalTokens,
		"TotalTokens should match upstream audio usage (not estimated)")
}

// TestOaiStreamHandler_AudioModelDetection_CallerModelContainsAudio verifies
// that when the caller model itself contains "audio", usage is also extracted
// correctly. This serves as a control case ensuring both paths converge.
func TestOaiStreamHandler_AudioModelDetection_CallerModelContainsAudio(t *testing.T) {
	gin.SetMode(gin.TestMode)

	oldStreamingTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 300
	t.Cleanup(func() { constant.StreamingTimeout = oldStreamingTimeout })

	callerModel := "gpt-4o-audio-preview"
	upstreamModel := "gpt-4o-audio-preview"

	upstreamUsage := dto.Usage{
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
	}

	sseBody := buildAudioSSE(upstreamModel, upstreamUsage)
	resp := &http.Response{
		Body:   nopCloser{strings.NewReader(string(sseBody))},
		Header: make(http.Header),
	}
	resp.Header.Set("Content-Type", "text/event-stream")

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	info := &relaycommon.RelayInfo{
		RelayFormat:     types.RelayFormatOpenAI,
		OriginModelName: callerModel,
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: upstreamModel,
		},
		RelayMode: relayconstant.RelayModeChatCompletions,
	}
	info.SetEstimatePromptTokens(10)

	usage, apiErr := OaiStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)

	require.Equal(t, 100, usage.PromptTokens)
	require.Equal(t, 50, usage.CompletionTokens)
	require.Equal(t, 150, usage.TotalTokens)
}

// TestOaiStreamHandler_NonAudioModel_SkipsSecondLastUsage verifies that for a
// non-audio model, the second-to-last usage is NOT extracted (the fallback
// text-based estimation path is used instead).
func TestOaiStreamHandler_NonAudioModel_SkipsSecondLastUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	oldStreamingTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 300
	t.Cleanup(func() { constant.StreamingTimeout = oldStreamingTimeout })

	callerModel := "gpt-4o"
	upstreamModel := "gpt-4o"

	upstreamUsage := dto.Usage{
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
	}

	sseBody := buildAudioSSE(upstreamModel, upstreamUsage)
	resp := &http.Response{
		Body:   nopCloser{strings.NewReader(string(sseBody))},
		Header: make(http.Header),
	}
	resp.Header.Set("Content-Type", "text/event-stream")

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	info := &relaycommon.RelayInfo{
		RelayFormat:     types.RelayFormatOpenAI,
		OriginModelName: callerModel,
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: upstreamModel,
		},
		RelayMode: relayconstant.RelayModeChatCompletions,
	}
	info.SetEstimatePromptTokens(10)

	usage, apiErr := OaiStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)

	// Non-audio model should NOT use the second-to-last chunk's usage;
	// it falls through to text-based estimation or last-chunk usage.
	require.Equal(t, 10, usage.PromptTokens,
		"Non-audio model should use estimated prompt tokens, not second-to-last usage")
}

// requireAllChunksUseCallerModel parses an SSE response body and asserts every
// data chunk's top-level model field equals the caller's requested model.
func requireAllChunksUseCallerModel(t *testing.T, body, callerModel string) {
	t.Helper()
	lines := strings.Split(body, "\n")
	chunks := 0
	for _, line := range lines {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		model := gjson.Get(strings.TrimPrefix(line, "data: "), "model").String()
		require.Equal(t, callerModel, model, "stream chunk should report the caller model")
		chunks++
	}
	require.Greater(t, chunks, 0, "expected at least one stream chunk")
}

// TestOaiStreamHandlerRewritesModelToCallerModel verifies that every streamed
// chunk reports the caller's model on all three sendStreamData branches — raw
// passthrough (default channel settings), ForceFormat, and ThinkingToContent —
// so a mapped channel never leaks the upstream model name mid-stream.
func TestOaiStreamHandlerRewritesModelToCallerModel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	oldStreamingTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 300
	t.Cleanup(func() { constant.StreamingTimeout = oldStreamingTimeout })

	callerModel := "my-fast-model"
	upstreamModel := "gpt-4o-2024-11-20"

	buildSSE := func() string {
		return "data: " + fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"%s","choices":[{"index":0,"delta":{"content":"Hel"}}]}`, upstreamModel) + "\n" +
			"data: " + fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"%s","choices":[{"index":0,"delta":{"reasoning_content":"thinking"}}]}`, upstreamModel) + "\n" +
			"data: " + fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"%s","choices":[{"index":0,"delta":{"content":"lo"}}]}`, upstreamModel) + "\n" +
			"data: " + fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"%s","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`, upstreamModel) + "\n" +
			"data: [DONE]\n"
	}

	for _, tc := range []struct {
		name              string
		forceFormat       bool
		thinkingToContent bool
	}{
		{name: "raw passthrough"},
		{name: "force format", forceFormat: true},
		{name: "thinking to content", thinkingToContent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{
				Body:   nopCloser{strings.NewReader(buildSSE())},
				Header: make(http.Header),
			}
			resp.Header.Set("Content-Type", "text/event-stream")

			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

			info := &relaycommon.RelayInfo{
				RelayFormat:     types.RelayFormatOpenAI,
				OriginModelName: callerModel,
				ChannelMeta: &relaycommon.ChannelMeta{
					UpstreamModelName: upstreamModel,
					ChannelSetting: dto.ChannelSettings{
						ForceFormat:       tc.forceFormat,
						ThinkingToContent: tc.thinkingToContent,
					},
				},
				RelayMode: relayconstant.RelayModeChatCompletions,
			}
			info.SetEstimatePromptTokens(5)

			_, apiErr := OaiStreamHandler(c, info, resp)
			require.Nil(t, apiErr, "OaiStreamHandler should not return an error")

			requireAllChunksUseCallerModel(t, rec.Body.String(), callerModel)
		})
	}
}

// TestOaiStreamHandlerPreservesMissingModelField verifies field-presence
// semantics: an upstream chunk without a model field must not gain one on the
// re-serialization paths (ForceFormat / ThinkingToContent), matching the raw
// passthrough branch's no-op behavior.
func TestOaiStreamHandlerPreservesMissingModelField(t *testing.T) {
	gin.SetMode(gin.TestMode)

	oldStreamingTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 300
	t.Cleanup(func() { constant.StreamingTimeout = oldStreamingTimeout })

	callerModel := "my-fast-model"
	upstreamModel := "gpt-4o-2024-11-20"

	sseBody := "data: " + fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"%s","choices":[{"index":0,"delta":{"content":"Hel"}}]}`, upstreamModel) + "\n" +
		"data: " + `{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"lo"}}]}` + "\n" +
		"data: [DONE]\n"

	for _, tc := range []struct {
		name        string
		forceFormat bool
	}{
		{name: "raw passthrough"},
		{name: "force format", forceFormat: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{
				Body:   nopCloser{strings.NewReader(sseBody)},
				Header: make(http.Header),
			}
			resp.Header.Set("Content-Type", "text/event-stream")

			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

			info := &relaycommon.RelayInfo{
				RelayFormat:     types.RelayFormatOpenAI,
				OriginModelName: callerModel,
				ChannelMeta: &relaycommon.ChannelMeta{
					UpstreamModelName: upstreamModel,
					ChannelSetting:    dto.ChannelSettings{ForceFormat: tc.forceFormat},
				},
				RelayMode: relayconstant.RelayModeChatCompletions,
			}
			info.SetEstimatePromptTokens(5)

			_, apiErr := OaiStreamHandler(c, info, resp)
			require.Nil(t, apiErr, "OaiStreamHandler should not return an error")

			// The DTO's Model field has no omitempty, so the re-serialization
			// branches always emit the field; the raw passthrough branch keeps
			// the chunk exactly as upstream sent it. In both cases a chunk whose
			// upstream payload lacked a model must NOT report the caller model.
			for _, line := range strings.Split(rec.Body.String(), "\n") {
				if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
					continue
				}
				model := gjson.Get(strings.TrimPrefix(line, "data: "), "model")
				if strings.Contains(line, `"content":"lo"`) {
					if model.Exists() {
						require.Equal(t, "", model.String(),
							"a chunk without an upstream model must not gain the caller model (empty DTO value matches pre-change shape)")
					}
				} else {
					require.Equal(t, callerModel, model.String(),
						"a chunk carrying an upstream model should report the caller model")
				}
			}
		})
	}
}

// TestOpenaiHandlerRewritesModelToCallerModel verifies the non-stream OpenAI
// chat completion response rewrites its top-level model to the caller's model.
func TestOpenaiHandlerRewritesModelToCallerModel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	callerModel := "my-fast-model"
	upstreamModel := "gpt-4o-2024-11-20"

	body := fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion","model":"%s","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`, upstreamModel)
	resp := &http.Response{
		Body:       nopCloser{strings.NewReader(body)},
		Header:     make(http.Header),
		StatusCode: http.StatusOK,
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	info := &relaycommon.RelayInfo{
		RelayFormat:     types.RelayFormatOpenAI,
		OriginModelName: callerModel,
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: upstreamModel,
		},
		RelayMode: relayconstant.RelayModeChatCompletions,
	}
	info.SetEstimatePromptTokens(5)

	usage, apiErr := OpenaiHandler(c, info, resp)
	require.Nil(t, apiErr, "OpenaiHandler should not return an error")
	require.NotNil(t, usage)
	require.Equal(t, callerModel, gjson.Get(rec.Body.String(), "model").String(),
		"non-stream response should report the caller model, not the mapped upstream model")
}
