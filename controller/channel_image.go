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
// adapter type with a non-empty image execution config). For non-image
// channels the builder returns (nil, nil) so the transactional save skips
// revision creation. An unknown channel type is a hard error (fail-closed)
// rather than a silent skip, surfacing a save failure if config somehow
// reached this point without validateImageExecutionConfig catching it.
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
			return nil, nil
		}
		version, _ := service.ImageAdapterVersion(apiType)
		input, err := channel.BuildImageChannelRevision(version)
		if err != nil {
			return nil, err
		}
		return &input, nil
	}
}
