package dto

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeImageTaskEditRequest(t *testing.T) {
	httpsURL := "https://cdn.example.com/a.png"

	tests := []struct {
		name    string
		body    string
		wantErr bool
		check   func(t *testing.T, req ImageTaskEditRequest)
	}{
		{
			name: "valid minimal one image",
			body: `{"model":"gpt-image-1","prompt":"extend the background","images":[{"image_url":"` + httpsURL + `"}]}`,
			check: func(t *testing.T, req ImageTaskEditRequest) {
				require.Len(t, req.Images, 1)
				assert.Equal(t, httpsURL, req.Images[0].ImageURL)
			},
		},
		{
			name: "valid eight images",
			body: `{"model":"gpt-image-1","prompt":"p","images":[` + imageArrayJSON(httpsURL, 8) + `]}`,
			check: func(t *testing.T, req ImageTaskEditRequest) {
				assert.Len(t, req.Images, 8)
			},
		},
		{
			name: "duplicate urls permitted and order preserved",
			body: `{"model":"gpt-image-1","prompt":"p","images":[{"image_url":"` + httpsURL + `"},{"image_url":"` + httpsURL + `"}]}`,
			check: func(t *testing.T, req ImageTaskEditRequest) {
				require.Len(t, req.Images, 2)
				assert.Equal(t, req.Images[0].ImageURL, req.Images[1].ImageURL)
			},
		},
		{
			name:    "nine images rejected not truncated",
			body:    `{"model":"gpt-image-1","prompt":"p","images":[` + imageArrayJSON(httpsURL, 9) + `]}`,
			wantErr: true,
		},
		{
			name:    "empty images array rejected",
			body:    `{"model":"gpt-image-1","prompt":"p","images":[]}`,
			wantErr: true,
		},
		{
			name:    "images absent rejected",
			body:    `{"model":"gpt-image-1","prompt":"p"}`,
			wantErr: true,
		},
		{
			name:    "http url rejected",
			body:    `{"model":"gpt-image-1","prompt":"p","images":[{"image_url":"http://cdn.example.com/a.png"}]}`,
			wantErr: true,
		},
		{
			name:    "relative url rejected",
			body:    `{"model":"gpt-image-1","prompt":"p","images":[{"image_url":"/a.png"}]}`,
			wantErr: true,
		},
		{
			name:    "empty image url rejected",
			body:    `{"model":"gpt-image-1","prompt":"p","images":[{"image_url":""}]}`,
			wantErr: true,
		},
		{
			name:    "oversized image url rejected",
			body:    `{"model":"gpt-image-1","prompt":"p","images":[{"image_url":"https://x.example.com/` + strings.Repeat("a", 8193) + `"}]}`,
			wantErr: true,
		},
		{
			name:    "image item extra field rejected",
			body:    `{"model":"gpt-image-1","prompt":"p","images":[{"image_url":"` + httpsURL + `","extra":1}]}`,
			wantErr: true,
		},
		{
			name:    "duplicate nested key in image object rejected",
			body:    `{"model":"gpt-image-1","prompt":"p","images":[{"image_url":"` + httpsURL + `","image_url":"` + httpsURL + `"}]}`,
			wantErr: true,
		},
		{
			name:    "explicit n rejected",
			body:    `{"model":"gpt-image-1","prompt":"p","images":[{"image_url":"` + httpsURL + `"}],"n":2}`,
			wantErr: true,
		},
		{
			name:    "excluded field style rejected",
			body:    `{"model":"gpt-image-1","prompt":"p","images":[{"image_url":"` + httpsURL + `"}],"style":"vivid"}`,
			wantErr: true,
		},
		{
			name:    "missing model rejected",
			body:    `{"prompt":"p","images":[{"image_url":"` + httpsURL + `"}]}`,
			wantErr: true,
		},
		{
			name:    "missing prompt rejected",
			body:    `{"model":"gpt-image-1","images":[{"image_url":"` + httpsURL + `"}]}`,
			wantErr: true,
		},
		{
			name:    "quality empty rejected",
			body:    `{"model":"gpt-image-1","prompt":"p","quality":"","images":[{"image_url":"` + httpsURL + `"}]}`,
			wantErr: true,
		},
		{
			name:    "duplicate top-level key rejected",
			body:    `{"model":"gpt-image-1","prompt":"p","images":[{"image_url":"` + httpsURL + `"}],"prompt":"q"}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := DecodeImageTaskEditRequest([]byte(tt.body))
			if tt.wantErr {
				rt := AsImageTaskRequestError(err)
				require.NotNil(t, rt, "expected ImageTaskRequestError, got %v", err)
				assert.Equal(t, ImageTaskErrInvalidRequest, rt.Code)
				assert.Equal(t, 400, rt.StatusCode)
				assert.NotEmpty(t, rt.Message)
				return
			}
			require.NoError(t, err)
			if tt.check != nil {
				tt.check(t, req)
			}
		})
	}
}

// imageArrayJSON builds a comma-joined list of n identical image objects for
// the boundary cases, keeping the test bodies deterministic.
func imageArrayJSON(url string, n int) string {
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		parts[i] = `{"image_url":"` + url + `"}`
	}
	return strings.Join(parts, ",")
}
