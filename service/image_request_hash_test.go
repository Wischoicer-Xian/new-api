package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanonicalImageRequestHash_EquivalenceAndConflict(t *testing.T) {
	tests := []struct {
		name      string
		left      []byte
		right     []byte
		wantEqual bool
	}{
		{
			name:      "object key order is irrelevant",
			left:      []byte(`{"model":"gpt-image-2","prompt":"a cat","size":"1024x1024"}`),
			right:     []byte(`{"size":"1024x1024","prompt":"a cat","model":"gpt-image-2"}`),
			wantEqual: true,
		},
		{name: "whitespace is irrelevant", left: []byte(`{"model":"m","prompt":"a cat"}`), right: []byte(`{ "model" : "m", "prompt" : "a cat" }`), wantEqual: true},
		{name: "nested key order is irrelevant", left: []byte(`{"meta":{"b":2,"a":1},"model":"m"}`), right: []byte(`{"model":"m","meta":{"a":1,"b":2}}`), wantEqual: true},
		{name: "equivalent decimal forms match", left: []byte(`{"value":1}`), right: []byte(`{"value":1.0}`), wantEqual: true},
		{name: "equivalent exponent forms match", left: []byte(`{"value":1000}`), right: []byte(`{"value":1.0e3}`), wantEqual: true},
		{name: "different prompt conflicts", left: []byte(`{"prompt":"a cat"}`), right: []byte(`{"prompt":"a dog"}`), wantEqual: false},
		{name: "array order is semantic", left: []byte(`{"refs":["x","y"]}`), right: []byte(`{"refs":["y","x"]}`), wantEqual: false},
		{name: "large integers remain distinct", left: []byte(`{"seed":9007199254740992}`), right: []byte(`{"seed":9007199254740993}`), wantEqual: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			leftHash, err := CanonicalImageRequestHash(test.left)
			require.NoError(t, err)
			rightHash, err := CanonicalImageRequestHash(test.right)
			require.NoError(t, err)
			if test.wantEqual {
				assert.Equal(t, leftHash, rightHash)
				return
			}
			assert.NotEqual(t, leftHash, rightHash)
		})
	}
}

func TestCanonicalImageRequestHash_RejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{name: "empty body", body: nil},
		{name: "malformed JSON", body: []byte(`{not json`)},
		{name: "non-object root", body: []byte(`["image"]`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := CanonicalImageRequestHash(test.body)
			require.Error(t, err)
		})
	}
}

func TestCanonicalImageRequestHash_HasHexSHA256Shape(t *testing.T) {
	hash, err := CanonicalImageRequestHash([]byte(`{"model":"m"}`))
	require.NoError(t, err)
	assert.Len(t, hash, 64)
	for _, char := range hash {
		assert.True(t, char >= '0' && char <= '9' || char >= 'a' && char <= 'f')
	}
}
