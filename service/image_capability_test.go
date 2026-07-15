package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubCaps struct {
	support  map[ImageOperation][]ImageExecutionMode
	defaults map[ImageOperation]ImageExecutionMode
}

func (s stubCaps) ImageTaskExecutionSupport(op ImageOperation) []ImageExecutionMode {
	return s.support[op]
}

func (s stubCaps) ImageTaskDefaultExecution(op ImageOperation) (ImageExecutionMode, bool) {
	mode, ok := s.defaults[op]
	return mode, ok
}

func caps(support map[ImageOperation][]ImageExecutionMode, defaults map[ImageOperation]ImageExecutionMode) stubCaps {
	return stubCaps{support: support, defaults: defaults}
}

func cfg(defaults map[ImageOperation]ImageExecutionMode, models map[string]map[ImageOperation]ImageExecutionMode) ImageChannelExecutionConfig {
	return ImageChannelExecutionConfig{Defaults: defaults, Models: models}
}

func TestResolveImageExecution(t *testing.T) {
	tests := []struct {
		name   string
		caps   stubCaps
		config ImageChannelExecutionConfig
		op     ImageOperation
		model  string
		want   ImageCapabilityResolution
		wantOK bool
	}{
		{
			name: "model override wins",
			caps: caps(
				map[ImageOperation][]ImageExecutionMode{ImageOperationGeneration: {ImageExecutionSync, ImageExecutionAsyncTask}},
				map[ImageOperation]ImageExecutionMode{ImageOperationGeneration: ImageExecutionSync},
			),
			config: cfg(
				map[ImageOperation]ImageExecutionMode{ImageOperationGeneration: ImageExecutionSync},
				map[string]map[ImageOperation]ImageExecutionMode{"gpt-image-2": {ImageOperationGeneration: ImageExecutionAsyncTask}},
			),
			op: ImageOperationGeneration, model: "gpt-image-2",
			want: ImageCapabilityResolution{Mode: ImageExecutionAsyncTask, Source: ImageCapabilitySourceModelOverride}, wantOK: true,
		},
		{
			name: "channel default when model absent",
			caps: caps(
				map[ImageOperation][]ImageExecutionMode{ImageOperationGeneration: {ImageExecutionSync, ImageExecutionAsyncTask}},
				map[ImageOperation]ImageExecutionMode{ImageOperationGeneration: ImageExecutionSync},
			),
			config: cfg(map[ImageOperation]ImageExecutionMode{ImageOperationGeneration: ImageExecutionAsyncTask}, nil),
			op:     ImageOperationGeneration, model: "ordinary",
			want: ImageCapabilityResolution{Mode: ImageExecutionAsyncTask, Source: ImageCapabilitySourceChannelDefault}, wantOK: true,
		},
		{
			name: "explicit adapter default is not support order",
			caps: caps(
				map[ImageOperation][]ImageExecutionMode{ImageOperationEdit: {ImageExecutionSync, ImageExecutionAsyncTask}},
				map[ImageOperation]ImageExecutionMode{ImageOperationEdit: ImageExecutionAsyncTask},
			),
			config: cfg(nil, nil), op: ImageOperationEdit, model: "m",
			want: ImageCapabilityResolution{Mode: ImageExecutionAsyncTask, Source: ImageCapabilitySourceAdapterDefault}, wantOK: true,
		},
		{
			name: "generation and edit resolve independently",
			caps: caps(
				map[ImageOperation][]ImageExecutionMode{ImageOperationGeneration: {ImageExecutionSync}, ImageOperationEdit: {ImageExecutionAsyncTask}},
				map[ImageOperation]ImageExecutionMode{ImageOperationGeneration: ImageExecutionSync, ImageOperationEdit: ImageExecutionAsyncTask},
			),
			config: cfg(map[ImageOperation]ImageExecutionMode{ImageOperationEdit: ImageExecutionAsyncTask}, nil),
			op:     ImageOperationGeneration, model: "m",
			want: ImageCapabilityResolution{Mode: ImageExecutionSync, Source: ImageCapabilitySourceAdapterDefault}, wantOK: true,
		},
		{
			name: "override for another model falls through",
			caps: caps(
				map[ImageOperation][]ImageExecutionMode{ImageOperationGeneration: {ImageExecutionSync, ImageExecutionAsyncTask}},
				map[ImageOperation]ImageExecutionMode{ImageOperationGeneration: ImageExecutionSync},
			),
			config: cfg(
				map[ImageOperation]ImageExecutionMode{ImageOperationGeneration: ImageExecutionSync},
				map[string]map[ImageOperation]ImageExecutionMode{"special": {ImageOperationGeneration: ImageExecutionAsyncTask}},
			),
			op: ImageOperationGeneration, model: "ordinary",
			want: ImageCapabilityResolution{Mode: ImageExecutionSync, Source: ImageCapabilitySourceChannelDefault}, wantOK: true,
		},
		{
			name:   "model override outside support fails closed",
			caps:   caps(map[ImageOperation][]ImageExecutionMode{ImageOperationGeneration: {ImageExecutionSync}}, map[ImageOperation]ImageExecutionMode{ImageOperationGeneration: ImageExecutionSync}),
			config: cfg(nil, map[string]map[ImageOperation]ImageExecutionMode{"m": {ImageOperationGeneration: ImageExecutionAsyncTask}}),
			op:     ImageOperationGeneration, model: "m", wantOK: false,
		},
		{
			name:   "channel default outside support fails closed",
			caps:   caps(map[ImageOperation][]ImageExecutionMode{ImageOperationGeneration: {ImageExecutionSync}}, map[ImageOperation]ImageExecutionMode{ImageOperationGeneration: ImageExecutionSync}),
			config: cfg(map[ImageOperation]ImageExecutionMode{ImageOperationGeneration: ImageExecutionAsyncTask}, nil),
			op:     ImageOperationGeneration, model: "m", wantOK: false,
		},
		{
			name:   "unsupported operation fails closed",
			caps:   caps(map[ImageOperation][]ImageExecutionMode{ImageOperationGeneration: {ImageExecutionSync}}, map[ImageOperation]ImageExecutionMode{ImageOperationGeneration: ImageExecutionSync}),
			config: cfg(nil, nil), op: ImageOperationEdit, model: "m", wantOK: false,
		},
		{
			name:   "invalid adapter default fails closed",
			caps:   caps(map[ImageOperation][]ImageExecutionMode{ImageOperationGeneration: {ImageExecutionSync}}, map[ImageOperation]ImageExecutionMode{ImageOperationGeneration: ImageExecutionAsyncTask}),
			config: cfg(nil, nil), op: ImageOperationGeneration, model: "m", wantOK: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := ResolveImageExecution(test.caps, test.config, test.op, test.model)
			require.Equal(t, test.wantOK, ok)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestValidateImageChannelExecutionConfig(t *testing.T) {
	tests := []struct {
		name    string
		caps    stubCaps
		config  ImageChannelExecutionConfig
		wantErr bool
	}{
		{
			name: "supported defaults and overrides",
			caps: caps(
				map[ImageOperation][]ImageExecutionMode{ImageOperationGeneration: {ImageExecutionSync, ImageExecutionAsyncTask}, ImageOperationEdit: {ImageExecutionAsyncTask}},
				map[ImageOperation]ImageExecutionMode{ImageOperationGeneration: ImageExecutionSync, ImageOperationEdit: ImageExecutionAsyncTask},
			),
			config: cfg(
				map[ImageOperation]ImageExecutionMode{ImageOperationGeneration: ImageExecutionAsyncTask, ImageOperationEdit: ImageExecutionAsyncTask},
				map[string]map[ImageOperation]ImageExecutionMode{"legacy": {ImageOperationGeneration: ImageExecutionSync}},
			),
		},
		{
			name:   "unsupported default",
			caps:   caps(map[ImageOperation][]ImageExecutionMode{ImageOperationGeneration: {ImageExecutionSync}}, map[ImageOperation]ImageExecutionMode{ImageOperationGeneration: ImageExecutionSync}),
			config: cfg(map[ImageOperation]ImageExecutionMode{ImageOperationGeneration: ImageExecutionAsyncTask}, nil), wantErr: true,
		},
		{
			name:   "unsupported override",
			caps:   caps(map[ImageOperation][]ImageExecutionMode{ImageOperationGeneration: {ImageExecutionAsyncTask}}, map[ImageOperation]ImageExecutionMode{ImageOperationGeneration: ImageExecutionAsyncTask}),
			config: cfg(nil, map[string]map[ImageOperation]ImageExecutionMode{"m": {ImageOperationGeneration: ImageExecutionSync}}), wantErr: true,
		},
		{
			name:   "override for unsupported operation",
			caps:   caps(map[ImageOperation][]ImageExecutionMode{ImageOperationGeneration: {ImageExecutionSync}}, map[ImageOperation]ImageExecutionMode{ImageOperationGeneration: ImageExecutionSync}),
			config: cfg(nil, map[string]map[ImageOperation]ImageExecutionMode{"m": {ImageOperationEdit: ImageExecutionAsyncTask}}), wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateImageChannelExecutionConfig(test.caps, test.config)
			if test.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}
