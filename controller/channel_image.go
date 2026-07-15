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
// image task candidate. A channel whose API type has not opted into the image
// task subsystem, or whose configured mode lies outside the adapter support
// set, is rejected so it can never silently enter the candidate pool. This is
// the fail-closed gate shared by AddChannel and UpdateChannel.
func validateImageExecutionConfig(channel *model.Channel) error {
	raw := channel.ImageExecutionConfigBytes()
	if len(raw) == 0 {
		return nil
	}
	cfg, err := service.ParseImageChannelExecutionConfig(raw)
	if err != nil {
		return fmt.Errorf("图片执行配置[image_execution_config] 格式错误：%s", err.Error())
	}
	apiType, _ := common.ChannelType2APIType(channel.Type)
	caps, ok := service.ImageAdapterCapabilities(apiType)
	if !ok {
		return fmt.Errorf("图片执行配置[image_execution_config] 该渠道类型不支持图片任务执行")
	}
	if err := service.ValidateImageChannelExecutionConfig(caps, cfg); err != nil {
		return fmt.Errorf("图片执行配置[image_execution_config] 无效：%s", err.Error())
	}
	return nil
}

// imageChannelAdapterVersion resolves the adapter version label for a channel,
// reporting whether the channel's API type is image-capable. Non-image-capable
// channels get an empty label and a false result.
func imageChannelAdapterVersion(channel *model.Channel) (string, bool) {
	apiType, _ := common.ChannelType2APIType(channel.Type)
	if _, ok := service.ImageAdapterCapabilities(apiType); !ok {
		return "", false
	}
	return service.ImageAdapterVersion(apiType), true
}

// ensureImageChannelRevision freezes an immutable revision for an image-capable
// channel after a successful config update, per the design's "渠道配置更新创建新
// revision" contract. Non-image channels are a no-op. A revision creation
// failure does not roll back the already-durable channel write; it is logged so
// an operator can reconcile, and the image task processor fail-closes for the
// channel until a revision exists.
func ensureImageChannelRevision(channel *model.Channel) {
	if len(channel.ImageExecutionConfigBytes()) == 0 {
		return
	}
	adapterVersion, ok := imageChannelAdapterVersion(channel)
	if !ok {
		return
	}
	if _, err := model.CreateChannelRevision(channel.BuildImageChannelRevisionInput(adapterVersion)); err != nil {
		common.SysError(fmt.Sprintf("为渠道 %d 创建图片 channel revision 失败：%s", channel.Id, err.Error()))
	}
}
