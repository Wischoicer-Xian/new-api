package zhipu_4v

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/dto"
	"github.com/gin-gonic/gin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
)

func TestConvertImageRequest_SanitizesDALLEQuality(t *testing.T) {
	adaptor := &Adaptor{}
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodPost, "/", nil)
	info := &relaycommon.RelayInfo{}

	tests := []struct {
		name     string
		quality  string
		expected string
	}{
		{"standard cleared", "standard", ""},
		{"hd cleared", "hd", ""},
		{"low preserved", "low", "low"},
		{"medium preserved", "medium", "medium"},
		{"high preserved", "high", "high"},
		{"auto preserved", "auto", "auto"},
		{"empty preserved", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := dto.ImageRequest{
				Model:   "cogview-4",
				Prompt:  "test prompt",
				Quality: tt.quality,
			}
			result, err := adaptor.ConvertImageRequest(c, info, req)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			imgReq, ok := result.(dto.ImageRequest)
			if !ok {
				t.Fatalf("expected dto.ImageRequest, got %T", result)
			}
			if imgReq.Quality != tt.expected {
				t.Errorf("quality: got %q, want %q", imgReq.Quality, tt.expected)
			}
		})
	}
}
