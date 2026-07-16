package dto_test

import (
	"testing"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CanonicalImageRequestHash is computed on the raw request body. These tests
// prove the §6.1 hash invariants hold for the bodies the DTO accepts: JSON
// key order is irrelevant, but images[] element order participates (reordering
// inputs is a semantic change) and duplicate URLs are not collapsed.

// hashOrDecodeFail decodes the body through the §6.1 strict DTO (asserting it
// is a contract-valid body) and then hashes the same raw bytes. A body that
// fails strict decode never reaches the idempotency layer in production.
func hashOrDecodeFail(t *testing.T, body []byte, decode func([]byte) error) string {
	require.NoError(t, decode(body))
	h, err := service.CanonicalImageRequestHash(body)
	require.NoError(t, err)
	return h
}

func TestGenerationRequestHashKeyOrderInvariant(t *testing.T) {
	a := hashOrDecodeFail(t, []byte(`{"model":"gpt-image-1","prompt":"p","size":"1024x1024"}`),
		func(b []byte) error { _, err := dto.DecodeImageTaskGenerationRequest(b); return err })
	b := hashOrDecodeFail(t, []byte(`{"size":"1024x1024","prompt":"p","model":"gpt-image-1"}`),
		func(b []byte) error { _, err := dto.DecodeImageTaskGenerationRequest(b); return err })
	assert.Equal(t, a, b, "JSON key order must not affect the canonical hash")
}

func TestEditRequestHashImageOrderSensitive(t *testing.T) {
	first := `{"model":"gpt-image-1","prompt":"p","images":[{"image_url":"https://a.example.com/1.png"},{"image_url":"https://a.example.com/2.png"}]}`
	swapped := `{"model":"gpt-image-1","prompt":"p","images":[{"image_url":"https://a.example.com/2.png"},{"image_url":"https://a.example.com/1.png"}]}`

	h1 := hashOrDecodeFail(t, []byte(first),
		func(b []byte) error { _, err := dto.DecodeImageTaskEditRequest(b); return err })
	h2 := hashOrDecodeFail(t, []byte(swapped),
		func(b []byte) error { _, err := dto.DecodeImageTaskEditRequest(b); return err })
	assert.NotEqual(t, h1, h2, "images[] order must participate in the canonical hash")
}

func TestEditRequestHashDuplicateURLNotCollapsed(t *testing.T) {
	once := `{"model":"gpt-image-1","prompt":"p","images":[{"image_url":"https://a.example.com/x.png"}]}`
	twice := `{"model":"gpt-image-1","prompt":"p","images":[{"image_url":"https://a.example.com/x.png"},{"image_url":"https://a.example.com/x.png"}]}`

	h1 := hashOrDecodeFail(t, []byte(once),
		func(b []byte) error { _, err := dto.DecodeImageTaskEditRequest(b); return err })
	h2 := hashOrDecodeFail(t, []byte(twice),
		func(b []byte) error { _, err := dto.DecodeImageTaskEditRequest(b); return err })
	assert.NotEqual(t, h1, h2, "duplicate URLs are semantic and must not collapse in the hash")
}
