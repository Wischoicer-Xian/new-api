package service

import (
	"fmt"

	"github.com/QuantumNous/new-api/common"
)

// imageChannelExecutionConfigDTO is the JSON shape persisted in channel
// configuration. Operation and mode travel as strings on the wire so the
// admin layer stays schema-light; ParseImageChannelExecutionConfig is the
// single place that turns those strings into validated domain values.
type imageChannelExecutionConfigDTO struct {
	Defaults map[string]string            `json:"defaults,omitempty"`
	Models   map[string]map[string]string `json:"models,omitempty"`
}

// ParseImageChannelExecutionConfig decodes channel execution configuration
// from persisted JSON and rejects any unknown operation or execution-mode
// string. Unknown values are an error rather than being dropped so that a
// typo or a stale field name surfaces immediately instead of silently
// leaving the channel on the adapter default.
func ParseImageChannelExecutionConfig(raw []byte) (ImageChannelExecutionConfig, error) {
	var dto imageChannelExecutionConfigDTO
	if len(raw) > 0 {
		if err := common.UnmarshalStrict(raw, &dto); err != nil {
			return ImageChannelExecutionConfig{}, fmt.Errorf("parse image execution config: %w", err)
		}
	}
	cfg := ImageChannelExecutionConfig{Defaults: map[ImageOperation]ImageExecutionMode{}}
	for opStr, modeStr := range dto.Defaults {
		op, ok := parseImageOperation(opStr)
		if !ok {
			return ImageChannelExecutionConfig{}, fmt.Errorf("unknown image operation %q", opStr)
		}
		mode, ok := parseImageExecutionMode(modeStr)
		if !ok {
			return ImageChannelExecutionConfig{}, fmt.Errorf("unknown execution mode %q for operation %q", modeStr, opStr)
		}
		cfg.Defaults[op] = mode
	}
	if len(dto.Models) > 0 {
		cfg.Models = map[string]map[ImageOperation]ImageExecutionMode{}
		for model, perOp := range dto.Models {
			parsed := map[ImageOperation]ImageExecutionMode{}
			for opStr, modeStr := range perOp {
				op, ok := parseImageOperation(opStr)
				if !ok {
					return ImageChannelExecutionConfig{}, fmt.Errorf("unknown image operation %q for model %q", opStr, model)
				}
				mode, ok := parseImageExecutionMode(modeStr)
				if !ok {
					return ImageChannelExecutionConfig{}, fmt.Errorf("unknown execution mode %q for model %q operation %q", modeStr, model, opStr)
				}
				parsed[op] = mode
			}
			cfg.Models[model] = parsed
		}
	}
	return cfg, nil
}

func parseImageOperation(s string) (ImageOperation, bool) {
	switch ImageOperation(s) {
	case ImageOperationGeneration:
		return ImageOperationGeneration, true
	case ImageOperationEdit:
		return ImageOperationEdit, true
	}
	return "", false
}

func parseImageExecutionMode(s string) (ImageExecutionMode, bool) {
	switch ImageExecutionMode(s) {
	case ImageExecutionSync:
		return ImageExecutionSync, true
	case ImageExecutionAsyncTask:
		return ImageExecutionAsyncTask, true
	}
	return "", false
}
