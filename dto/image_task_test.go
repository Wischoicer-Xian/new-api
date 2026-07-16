package dto

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeImageTaskGenerationRequest(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr bool
		check   func(t *testing.T, req ImageTaskGenerationRequest)
	}{
		{
			name: "valid minimal",
			body: `{"model":"gpt-image-1","prompt":"a red panda"}`,
			check: func(t *testing.T, req ImageTaskGenerationRequest) {
				assert.Equal(t, "gpt-image-1", req.Model)
				assert.Nil(t, req.Quality)
				assert.Nil(t, req.Size)
			},
		},
		{
			name: "valid with quality and size",
			body: `{"model":"gpt-image-1","prompt":"p","quality":"hd","size":"1024x1024"}`,
			check: func(t *testing.T, req ImageTaskGenerationRequest) {
				require.NotNil(t, req.Quality)
				assert.Equal(t, "hd", *req.Quality)
				require.NotNil(t, req.Size)
				assert.Equal(t, "1024x1024", *req.Size)
			},
		},
		{
			name:    "explicit n rejected as unknown field",
			body:    `{"model":"gpt-image-1","prompt":"p","n":2}`,
			wantErr: true,
		},
		{
			name:    "excluded field style rejected",
			body:    `{"model":"gpt-image-1","prompt":"p","style":"vivid"}`,
			wantErr: true,
		},
		{
			name:    "excluded field response_format rejected",
			body:    `{"model":"gpt-image-1","prompt":"p","response_format":"url"}`,
			wantErr: true,
		},
		{
			name:    "excluded field watermark rejected",
			body:    `{"model":"gpt-image-1","prompt":"p","watermark":false}`,
			wantErr: true,
		},
		{
			name:    "duplicate top-level key rejected",
			body:    `{"model":"gpt-image-1","prompt":"p","model":"other"}`,
			wantErr: true,
		},
		{
			name:    "malformed json rejected",
			body:    `{"model":"gpt-image-1","prompt"`,
			wantErr: true,
		},
		{
			name:    "non-object top level rejected",
			body:    `["gpt-image-1"]`,
			wantErr: true,
		},
		{
			name:    "empty body rejected",
			body:    ``,
			wantErr: true,
		},
		{
			name:    "missing model rejected",
			body:    `{"prompt":"p"}`,
			wantErr: true,
		},
		{
			name:    "blank model rejected",
			body:    `{"model":"  ","prompt":"p"}`,
			wantErr: true,
		},
		{
			name:    "missing prompt rejected",
			body:    `{"model":"gpt-image-1"}`,
			wantErr: true,
		},
		{
			name:    "quality present but empty rejected",
			body:    `{"model":"gpt-image-1","prompt":"p","quality":""}`,
			wantErr: true,
		},
		{
			name:    "quality blank rejected",
			body:    `{"model":"gpt-image-1","prompt":"p","quality":"  "}`,
			wantErr: true,
		},
		{
			name:    "size present but empty rejected",
			body:    `{"model":"gpt-image-1","prompt":"p","size":""}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := DecodeImageTaskGenerationRequest([]byte(tt.body))
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

func TestValidateImageTaskContentType(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		wantStatus  int
	}{
		{name: "application/json accepted", contentType: "application/json", wantStatus: 0},
		{name: "application/json with charset accepted", contentType: "application/json; charset=utf-8", wantStatus: 0},
		{name: "multipart rejected", contentType: "multipart/form-data; boundary=xyz", wantStatus: 415},
		{name: "text/plain rejected", contentType: "text/plain", wantStatus: 415},
		{name: "empty content type rejected", contentType: "", wantStatus: 415},
		{name: "malformed content type rejected", contentType: "application/json; bad=(", wantStatus: 415},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateImageTaskContentType(tt.contentType)
			if tt.wantStatus == 0 {
				assert.NoError(t, err)
				return
			}
			rt := AsImageTaskRequestError(err)
			require.NotNil(t, err)
			require.NotNil(t, rt, "expected error for %q", tt.contentType)
			assert.Equal(t, ImageTaskErrUnsupportedMediaType, rt.Code)
			assert.Equal(t, tt.wantStatus, rt.StatusCode)
		})
	}
}

func TestValidateIdempotencyKey(t *testing.T) {
	emoji191 := strings.Repeat("🌟", 47) + "abc" // 47*4 + 3 = 191 bytes

	tests := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{name: "empty rejected", key: "", wantErr: true},
		{name: "one byte accepted", key: "a", wantErr: false},
		{name: "max 191 bytes accepted", key: strings.Repeat("a", 191), wantErr: false},
		{name: "max 191 multibyte bytes accepted", key: emoji191, wantErr: false},
		{name: "192 bytes rejected", key: strings.Repeat("a", 192), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateIdempotencyKey(tt.key)
			if tt.wantErr {
				rt := AsImageTaskRequestError(err)
				require.NotNil(t, rt, "expected error for key len %d", len(tt.key))
				assert.Equal(t, ImageTaskErrInvalidRequest, rt.Code)
				assert.Equal(t, 400, rt.StatusCode)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestAsImageTaskRequestError(t *testing.T) {
	t.Run("unwraps wrapped task error", func(t *testing.T) {
		inner := ValidateIdempotencyKey("")
		wrapped := errors.Join(errors.New("context"), inner)
		rt := AsImageTaskRequestError(wrapped)
		require.NotNil(t, rt)
		assert.Equal(t, ImageTaskErrInvalidRequest, rt.Code)
	})
	t.Run("non-task error returns nil", func(t *testing.T) {
		assert.Nil(t, AsImageTaskRequestError(errors.New("plain")))
	})
	t.Run("nil returns nil", func(t *testing.T) {
		assert.Nil(t, AsImageTaskRequestError(nil))
	})
}
