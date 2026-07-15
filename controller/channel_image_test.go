package controller

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
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
