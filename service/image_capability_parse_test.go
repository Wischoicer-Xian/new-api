package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseImageChannelExecutionConfig(t *testing.T) {
	tests := []struct {
		name    string
		raw     []byte
		want    ImageChannelExecutionConfig
		wantErr bool
	}{
		{
			name: "full config",
			raw: []byte(`{
			  "defaults": {"generation": "async_task", "edit": "async_task"},
			  "models": {"legacy-image": {"generation": "sync"}}
			}`),
			want: ImageChannelExecutionConfig{
				Defaults: map[ImageOperation]ImageExecutionMode{ImageOperationGeneration: ImageExecutionAsyncTask, ImageOperationEdit: ImageExecutionAsyncTask},
				Models:   map[string]map[ImageOperation]ImageExecutionMode{"legacy-image": {ImageOperationGeneration: ImageExecutionSync}},
			},
		},
		{name: "empty object", raw: []byte(`{}`), want: ImageChannelExecutionConfig{Defaults: map[ImageOperation]ImageExecutionMode{}}},
		{name: "nil input", raw: nil, want: ImageChannelExecutionConfig{Defaults: map[ImageOperation]ImageExecutionMode{}}},
		{name: "unknown operation", raw: []byte(`{"defaults":{"generaton":"sync"}}`), wantErr: true},
		{name: "unknown mode", raw: []byte(`{"defaults":{"generation":"batch"}}`), wantErr: true},
		{name: "unknown override mode", raw: []byte(`{"models":{"m":{"edit":"streaming"}}}`), wantErr: true},
		{name: "malformed JSON", raw: []byte(`{not json`), wantErr: true},
		{name: "unknown top-level field", raw: []byte(`{"default":{"generation":"sync"}}`), wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseImageChannelExecutionConfig(test.raw)
			if test.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}
