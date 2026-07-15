package controller

import (
	"net/http"
	"sort"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// imageCapabilityPreviewRequest is the admin-facing body for the image
// capability preview endpoint. The channel type drives adapter selection; the
// raw image_execution_config JSON is the in-edit configuration the admin wants
// resolved, so the preview reflects unsaved changes in the drawer rather than
// only the persisted channel.
type imageCapabilityPreviewRequest struct {
	Type                 int    `json:"type"`
	ImageExecutionConfig string `json:"image_execution_config"`
}

// imageCapabilitySupport is the adapter support set for the channel's API type,
// surfaced as operation -> execution modes so the UI can disable options the
// adapter does not implement.
type imageCapabilitySupport struct {
	Generation []string `json:"generation,omitempty"`
	Edit       []string `json:"edit,omitempty"`
}

// ImageCapabilityPreviewResponse is the resolved capability view for one
// channel type + in-edit configuration. Preview entries report the effective
// mode, its precedence source, and whether the resolution is fail-closed.
type ImageCapabilityPreviewResponse struct {
	ImageCapable   bool                                  `json:"image_capable"`
	AdapterVersion string                                `json:"adapter_version,omitempty"`
	Support        imageCapabilitySupport                `json:"support"`
	Preview        []service.ImageCapabilityPreviewEntry `json:"preview"`
}

func execModesToStrings(modes []service.ImageExecutionMode) []string {
	out := make([]string, 0, len(modes))
	for _, m := range modes {
		out = append(out, string(m))
	}
	return out
}

// ImageCapabilityPreview resolves the image task execution capability for a
// channel type and an in-edit execution configuration. It is a read-only
// computation used by the admin channel editor to render the effective mode,
// disable unsupported options, and flag fail-closed configurations before the
// channel is saved. It never reads provider state and exposes no provider,
// channel or task identifiers — only execution-mode metadata.
func ImageCapabilityPreview(c *gin.Context) {
	var req imageCapabilityPreviewRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiError(c, err)
		return
	}

	apiType, _ := common.ChannelType2APIType(req.Type)
	caps, ok := service.ImageAdapterCapabilities(apiType)
	if !ok {
		// Not image-capable: empty support, no preview. The UI uses this to
		// hide the configuration controls for this channel type.
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"data":    ImageCapabilityPreviewResponse{},
		})
		return
	}

	cfg, err := service.ParseImageChannelExecutionConfig([]byte(strings.TrimSpace(req.ImageExecutionConfig)))
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "图片执行配置[image_execution_config] 格式错误：" + err.Error(),
		})
		return
	}

	// Report every configured model override, in a stable order so the UI does
	// not flicker between requests.
	models := make([]string, 0, len(cfg.Models))
	for model := range cfg.Models {
		models = append(models, model)
	}
	sort.Strings(models)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": ImageCapabilityPreviewResponse{
			ImageCapable:   true,
			AdapterVersion: service.ImageAdapterVersion(apiType),
			Support: imageCapabilitySupport{
				Generation: execModesToStrings(caps.ImageTaskExecutionSupport(service.ImageOperationGeneration)),
				Edit:       execModesToStrings(caps.ImageTaskExecutionSupport(service.ImageOperationEdit)),
			},
			Preview: service.PreviewImageChannelExecution(caps, cfg, models),
		},
	})
}
