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
			name:    "image item extra field rejected",
			body:    `{"model":"gpt-image-1","prompt":"p","images":[{"image_url":"` + httpsURL + `","extra":1}]}`,
			wantErr: true,
		},
		{
			name:    "image item case variant Image_URL rejected",
			body:    `{"model":"gpt-image-1","prompt":"p","images":[{"Image_URL":"` + httpsURL + `"}]}`,
			wantErr: true,
		},
		{
			name:    "image item null image_url rejected",
			body:    `{"model":"gpt-image-1","prompt":"p","images":[{"image_url":null}]}`,
			wantErr: true,
		},
		{
			name:    "opaque https url rejected",
			body:    `{"model":"gpt-image-1","prompt":"p","images":[{"image_url":"https:opaque-value"}]}`,
			wantErr: true,
		},
		{
			name:    "hostless triple slash url rejected",
			body:    `{"model":"gpt-image-1","prompt":"p","images":[{"image_url":"https:///path"}]}`,
			wantErr: true,
		},
		{
			name:    "query only url rejected",
			body:    `{"model":"gpt-image-1","prompt":"p","images":[{"image_url":"https://?q=1"}]}`,
			wantErr: true,
		},
		{
			name:    "scheme only url rejected",
			body:    `{"model":"gpt-image-1","prompt":"p","images":[{"image_url":"https://"}]}`,
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
			name:    "quality null rejected",
			body:    `{"model":"gpt-image-1","prompt":"p","quality":null,"images":[{"image_url":"` + httpsURL + `"}]}`,
			wantErr: true,
		},
		{
			name:    "duplicate top-level key rejected",
			body:    `{"model":"gpt-image-1","prompt":"p","images":[{"image_url":"` + httpsURL + `"}],"prompt":"q"}`,
			wantErr: true,
		},
		{
			name: "url with explicit port accepted",
			body: `{"model":"gpt-image-1","prompt":"p","images":[{"image_url":"https://cdn.example.com:8443/a.png"}]}`,
			check: func(t *testing.T, req ImageTaskEditRequest) {
				require.Len(t, req.Images, 1)
				assert.Equal(t, "https://cdn.example.com:8443/a.png", req.Images[0].ImageURL)
			},
		},
		{
			name: "url with path and query accepted",
			body: `{"model":"gpt-image-1","prompt":"p","images":[{"image_url":"https://cdn.example.com/a/b.png?x=1&y=2"}]}`,
			check: func(t *testing.T, req ImageTaskEditRequest) {
				require.Len(t, req.Images, 1)
				assert.Equal(t, "https://cdn.example.com/a/b.png?x=1&y=2", req.Images[0].ImageURL)
			},
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

	// §12.4 image_url length cap (8192): accept at exactly the cap, reject one
	// byte over. Built from the constant so the boundary tracks the cap if it
	// ever changes.
	t.Run("image_url length boundary 8192 accept and 8193 reject", func(t *testing.T) {
		prefix := "https://x.example.com/"
		acceptURL := prefix + strings.Repeat("a", maxImageURLBytes-len(prefix))
		require.Equal(t, maxImageURLBytes, len(acceptURL))
		acceptBody := `{"model":"gpt-image-1","prompt":"p","images":[{"image_url":"` + acceptURL + `"}]}`
		req, err := DecodeImageTaskEditRequest([]byte(acceptBody))
		require.NoError(t, err)
		require.Len(t, req.Images, 1)
		assert.Len(t, req.Images[0].ImageURL, maxImageURLBytes)

		rejectURL := prefix + strings.Repeat("a", maxImageURLBytes-len(prefix)+1)
		rejectBody := `{"model":"gpt-image-1","prompt":"p","images":[{"image_url":"` + rejectURL + `"}]}`
		_, err = DecodeImageTaskEditRequest([]byte(rejectBody))
		rt := AsImageTaskRequestError(err)
		require.NotNil(t, rt)
		assert.Equal(t, ImageTaskErrInvalidRequest, rt.Code)
	})
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
