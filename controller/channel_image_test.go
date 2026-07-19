package controller

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateImageExecutionConfig(t *testing.T) {
	cfg := func(s string) *string { return &s }
	cases := []struct {
		name    string
		channel *model.Channel
		wantErr string
	}{
		{
			name:    "no config is valid and not image-capable",
			channel: &model.Channel{Type: constant.ChannelTypeOpenAI},
		},
		{
			name: "valid sync defaults on openai channel",
			channel: &model.Channel{
				Type:                 constant.ChannelTypeOpenAI,
				ImageExecutionConfig: cfg(`{"defaults":{"generation":"sync","edit":"sync"}}`),
			},
		},
		{
			name: "valid model override on openai channel",
			channel: &model.Channel{
				Type:                 constant.ChannelTypeOpenAI,
				ImageExecutionConfig: cfg(`{"models":{"gpt-image-1":{"generation":"sync"}}}`),
			},
		},
		{
			name: "unsupported adapter fail closed",
			channel: &model.Channel{
				Type:                 constant.ChannelTypeAnthropic,
				ImageExecutionConfig: cfg(`{"defaults":{"generation":"sync"}}`),
			},
			wantErr: "不支持图片任务执行",
		},
		{
			// P1-2: an unknown channel type must not be degraded to OpenAI; the
			// ChannelType2APIType mapping bool is honored and the type rejected.
			name: "unknown channel type rejected not degraded to openai",
			channel: &model.Channel{
				Type:                 99999,
				ImageExecutionConfig: cfg(`{"defaults":{"generation":"sync"}}`),
			},
			wantErr: "未知渠道类型",
		},
		{
			name: "unsupported mode fail closed",
			channel: &model.Channel{
				Type:                 constant.ChannelTypeOpenAI,
				ImageExecutionConfig: cfg(`{"defaults":{"generation":"async_task"}}`),
			},
			wantErr: "无效",
		},
		{
			name: "unknown operation rejected",
			channel: &model.Channel{
				Type:                 constant.ChannelTypeOpenAI,
				ImageExecutionConfig: cfg(`{"defaults":{"upscale":"sync"}}`),
			},
			wantErr: "格式错误",
		},
		{
			name: "malformed json rejected",
			channel: &model.Channel{
				Type:                 constant.ChannelTypeOpenAI,
				ImageExecutionConfig: cfg(`{"defaults":`),
			},
			wantErr: "格式错误",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateImageExecutionConfig(tc.channel)
			switch {
			case tc.wantErr == "":
				assert.NoError(t, err)
			case err == nil:
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			default:
				assert.True(t, strings.Contains(err.Error(), tc.wantErr), "err=%q want substr %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestImageChannelRevisionBuilder_FailClosedOnConfigWithUnsupportedAdapter(t *testing.T) {
	builder := imageChannelRevisionBuilder()
	cfg := `{"defaults":{"generation":"sync"}}`

	// non-empty config + supported adapter builds a revision
	rev, err := builder(&model.Channel{Id: 1, Type: constant.ChannelTypeOpenAI, ImageExecutionConfig: &cfg})
	require.NoError(t, err)
	require.NotNil(t, rev)

	// P1-1.3 fail-closed: non-empty config + unsupported adapter must ERROR,
	// not return (nil, nil). This is what blocks a partial type patch that
	// leaves a channel carrying a config it can no longer honor.
	_, err = builder(&model.Channel{Id: 2, Type: constant.ChannelTypeAnthropic, ImageExecutionConfig: &cfg})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "不支持图片任务执行")

	// empty config + unsupported adapter -> no revision, no error (not image-capable)
	rev, err = builder(&model.Channel{Id: 3, Type: constant.ChannelTypeAnthropic})
	require.NoError(t, err)
	assert.Nil(t, rev)
}

func ptrCfg(s string) *string { return &s }

// TestMergeAndValidate_FinalState covers P1-2: validation runs against the
// FINAL persisted object (origin merged with the patch by requestData key
// presence), not the patch-zero view. Before the fix a config-only patch was
// rejected as "未知渠道类型 0" before origin merge; now the merged final state
// is validated in one pass.
func TestMergeAndValidate_FinalState(t *testing.T) {
	cases := []struct {
		name        string
		origin      model.Channel
		requestData map[string]any
		patch       PatchChannel
		wantErr     string
	}{
		{
			name:   "config-only patch inherits OpenAI type and validates",
			origin: model.Channel{Type: constant.ChannelTypeOpenAI},
			requestData: map[string]any{
				"image_execution_config": `{"defaults":{"generation":"sync"}}`,
			},
			patch:   PatchChannel{Channel: model.Channel{ImageExecutionConfig: ptrCfg(`{"defaults":{"generation":"sync"}}`)}},
			wantErr: "",
		},
		{
			name:   "type-only patch to unsupported inherits old config and is rejected",
			origin: model.Channel{Type: constant.ChannelTypeOpenAI, ImageExecutionConfig: ptrCfg(`{"defaults":{"generation":"sync"}}`)},
			requestData: map[string]any{
				"type": float64(constant.ChannelTypeAnthropic),
			},
			patch:   PatchChannel{Channel: model.Channel{Type: constant.ChannelTypeAnthropic}},
			wantErr: "不支持图片任务执行",
		},
		{
			name:   "explicit clear config is honored not inherited from origin",
			origin: model.Channel{Type: constant.ChannelTypeOpenAI, ImageExecutionConfig: ptrCfg(`{"defaults":{"generation":"sync"}}`)},
			requestData: map[string]any{
				"image_execution_config": nil,
			},
			patch:   PatchChannel{Channel: model.Channel{Type: constant.ChannelTypeOpenAI}},
			wantErr: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := tc.patch
			mergeChannelPatchIntoOrigin(&ch, &tc.origin, tc.requestData)
			err := validateChannel(&ch.Channel, false)
			switch {
			case tc.wantErr == "":
				assert.NoError(t, err)
			case err == nil:
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			default:
				assert.Contains(t, err.Error(), tc.wantErr)
			}
		})
	}
}
