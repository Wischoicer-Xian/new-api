package service

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImageAdapterCapabilities_OpenAIRegistered(t *testing.T) {
	caps, ok := ImageAdapterCapabilities(constant.APITypeOpenAI)
	require.True(t, ok)
	assert.Equal(t, []ImageExecutionMode{ImageExecutionSync}, caps.ImageTaskExecutionSupport(ImageOperationGeneration))
	assert.Equal(t, []ImageExecutionMode{ImageExecutionSync}, caps.ImageTaskExecutionSupport(ImageOperationEdit))
	mode, ok := caps.ImageTaskDefaultExecution(ImageOperationGeneration)
	require.True(t, ok)
	assert.Equal(t, ImageExecutionSync, mode)
	mode, ok = caps.ImageTaskDefaultExecution(ImageOperationEdit)
	require.True(t, ok)
	assert.Equal(t, ImageExecutionSync, mode)
}

func TestImageAdapterCapabilities_FailClosed(t *testing.T) {
	// Adapters that have not opted into the image task subsystem must report
	// not-image-capable so channels of these types never enter the candidate
	// pool. Asserting the closed set guards against accidentally widening it.
	cases := []struct {
		name    string
		apiType int
	}{
		{"anthropic", constant.APITypeAnthropic},
		{"gemini", constant.APITypeGemini},
		{"ali", constant.APITypeAli},
		{"zhipu", constant.APITypeZhipu},
		{"jimeng", constant.APITypeJimeng},
		{"minimax", constant.APITypeMiniMax},
		{"unregistered sentinel", constant.APITypeDummy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := ImageAdapterCapabilities(tc.apiType)
			assert.False(t, ok, "apiType %d must not be image-capable", tc.apiType)
		})
	}
}

func TestImageAdapterVersion_StableLabel(t *testing.T) {
	assert.Equal(t, "apitype:0", ImageAdapterVersion(constant.APITypeOpenAI))
	assert.Equal(t, "apitype:99", ImageAdapterVersion(99))
}

func findImagePreview(preview []ImageCapabilityPreviewEntry, op ImageOperation, model string) *ImageCapabilityPreviewEntry {
	for i := range preview {
		if preview[i].Operation == op && preview[i].Model == model {
			return &preview[i]
		}
	}
	return nil
}

func TestPreviewImageChannelExecution(t *testing.T) {
	caps, _ := ImageAdapterCapabilities(constant.APITypeOpenAI)

	t.Run("empty config resolves adapter default for both operations", func(t *testing.T) {
		cfg := ImageChannelExecutionConfig{Defaults: map[ImageOperation]ImageExecutionMode{}}
		preview := PreviewImageChannelExecution(caps, cfg, nil)
		require.Len(t, preview, 2)
		for _, e := range preview {
			assert.True(t, e.Ok, "%s should resolve", e.Operation)
			assert.Equal(t, ImageExecutionSync, e.Mode)
			assert.Equal(t, ImageCapabilitySourceAdapterDefault, e.Source)
		}
	})

	t.Run("channel default reported with channel source", func(t *testing.T) {
		cfg := ImageChannelExecutionConfig{Defaults: map[ImageOperation]ImageExecutionMode{
			ImageOperationGeneration: ImageExecutionSync,
		}}
		preview := PreviewImageChannelExecution(caps, cfg, nil)
		gen := findImagePreview(preview, ImageOperationGeneration, "")
		require.NotNil(t, gen)
		assert.Equal(t, ImageCapabilitySourceChannelDefault, gen.Source)
		assert.True(t, gen.Ok)
	})

	t.Run("model override reported with model source", func(t *testing.T) {
		cfg := ImageChannelExecutionConfig{
			Models: map[string]map[ImageOperation]ImageExecutionMode{
				"gpt-image-1": {ImageOperationEdit: ImageExecutionSync},
			},
		}
		preview := PreviewImageChannelExecution(caps, cfg, []string{"gpt-image-1"})
		entry := findImagePreview(preview, ImageOperationEdit, "gpt-image-1")
		require.NotNil(t, entry)
		assert.Equal(t, ImageCapabilitySourceModelOverride, entry.Source)
		assert.True(t, entry.Ok)
	})

	t.Run("unsupported mode fail closed in preview", func(t *testing.T) {
		// async_task is not in the OpenAI support set; resolution must report
		// not-ok rather than silently degrading to a supported mode.
		cfg := ImageChannelExecutionConfig{Defaults: map[ImageOperation]ImageExecutionMode{
			ImageOperationGeneration: ImageExecutionAsyncTask,
		}}
		preview := PreviewImageChannelExecution(caps, cfg, nil)
		gen := findImagePreview(preview, ImageOperationGeneration, "")
		require.NotNil(t, gen)
		assert.False(t, gen.Ok)
	})

	t.Run("nil caps yields no preview", func(t *testing.T) {
		assert.Nil(t, PreviewImageChannelExecution(nil, ImageChannelExecutionConfig{}, nil))
	})
}
