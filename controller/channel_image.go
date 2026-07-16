package controller

import (
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
)

// validateImageExecutionConfig parses and validates the channel's image task
// execution configuration against the adapter support set for the channel's
// API type. A channel with no configuration is valid: it is simply not an
// image task candidate. An unknown channel type, an adapter that has not opted
// into the image task subsystem, or a configured mode outside the adapter
// support set is rejected so it can never silently enter the candidate pool.
// This is the fail-closed gate shared by AddChannel and UpdateChannel; the
// ChannelType2APIType mapping bool is checked first so an unknown type is
// never degraded to OpenAI.
func validateImageExecutionConfig(channel *model.Channel) error {
	raw := channel.ImageExecutionConfigBytes()
	if len(raw) == 0 {
		return nil
	}
	apiType, ok := common.ChannelType2APIType(channel.Type)
	if !ok {
		return fmt.Errorf("图片执行配置[image_execution_config] 未知渠道类型 %d", channel.Type)
	}
	caps, ok := service.ImageAdapterCapabilities(apiType)
	if !ok {
		return fmt.Errorf("图片执行配置[image_execution_config] 该渠道类型不支持图片任务执行")
	}
	cfg, err := service.ParseImageChannelExecutionConfig(raw)
	if err != nil {
		return fmt.Errorf("图片执行配置[image_execution_config] 格式错误：%s", err.Error())
	}
	if err := service.ValidateImageChannelExecutionConfig(caps, cfg); err != nil {
		return fmt.Errorf("图片执行配置[image_execution_config] 无效：%s", err.Error())
	}
	return nil
}

// imageChannelRevisionBuilder returns a channelRevisionBuilder that freezes an
// immutable revision for the channel when it is image-capable (a registered
// adapter type with a non-empty image execution config). For channels with no
// image config the builder returns (nil, nil) so the transactional save skips
// revision creation. A non-empty config paired with an unknown channel type OR
// an adapter that has not opted into the image task subsystem is a HARD error
// (fail-closed): the save must fail rather than persist a channel carrying an
// image config it cannot honor (which would later enter the candidate pool with
// no executable adapter and no revision).
func imageChannelRevisionBuilder() model.ChannelRevisionBuilder {
	return func(channel *model.Channel) (*model.ChannelRevisionCreate, error) {
		if len(channel.ImageExecutionConfigBytes()) == 0 {
			return nil, nil
		}
		apiType, ok := common.ChannelType2APIType(channel.Type)
		if !ok {
			return nil, fmt.Errorf("图片执行配置[image_execution_config] 未知渠道类型 %d", channel.Type)
		}
		if _, ok := service.ImageAdapterCapabilities(apiType); !ok {
			return nil, fmt.Errorf("图片执行配置[image_execution_config] 该渠道类型不支持图片任务执行")
		}
		version, _ := service.ImageAdapterVersion(apiType)
		input, err := channel.BuildImageChannelRevision(version)
		if err != nil {
			return nil, err
		}
		return &input, nil
	}
}

// mergeChannelPatchIntoOrigin fills patch fields the request omitted with the
// origin values, keyed on requestData presence. The resulting object reflects
// the FINAL persisted state so validation runs against it (not the patch-zero
// view). A field present in requestData (even null/empty) is honored as the
// patch intent and NOT overridden — that is what locks the "explicit clear"
// semantics (e.g. sending image_execution_config: null clears it). Only fields
// consumed by validateChannel or frozen into a revision are merged; other
// fields rely on GORM's zero-skip during Updates, unchanged from prior
// behavior.
func mergeChannelPatchIntoOrigin(channel *PatchChannel, origin *model.Channel, requestData map[string]any) {
	if _, ok := requestData["type"]; !ok {
		channel.Type = origin.Type
	}
	if _, ok := requestData["base_url"]; !ok {
		channel.BaseURL = origin.BaseURL
	}
	if _, ok := requestData["other"]; !ok {
		channel.Other = origin.Other
	}
	if _, ok := requestData["setting"]; !ok {
		channel.Setting = origin.Setting
	}
	if _, ok := requestData["image_execution_config"]; !ok {
		channel.ImageExecutionConfig = origin.ImageExecutionConfig
	}
	if _, ok := requestData["status_code_mapping"]; !ok {
		channel.StatusCodeMapping = origin.StatusCodeMapping
	}
	if _, ok := requestData["model_mapping"]; !ok {
		channel.ModelMapping = origin.ModelMapping
	}
	if _, ok := requestData["param_override"]; !ok {
		channel.ParamOverride = origin.ParamOverride
	}
	if _, ok := requestData["header_override"]; !ok {
		channel.HeaderOverride = origin.HeaderOverride
	}
	if _, ok := requestData["openai_organization"]; !ok {
		channel.OpenAIOrganization = origin.OpenAIOrganization
	}
}
