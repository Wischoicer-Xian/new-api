package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAssertJSONObjectNoDuplicateKeys(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		wantErr error
		wantDup string
	}{
		{name: "valid flat object", data: `{"model":"m","prompt":"p"}`},
		{name: "valid nested object", data: `{"model":"m","meta":{"a":1,"b":2}}`},
		{name: "valid array values", data: `{"tags":["a","b"],"nums":[1,2,3]}`},
		{name: "valid array of objects with same key per scope", data: `{"images":[{"image_url":"u1"},{"image_url":"u2"}]}`},
		{name: "same key across sibling scopes is allowed", data: `{"a":{"x":1},"b":{"x":2}}`},
		{name: "empty object", data: `{}`},
		{name: "malformed json", data: `{"model":"m","prompt"`, wantErr: ErrJSONMalformed},
		{name: "empty data", data: ``, wantErr: ErrJSONMalformed},
		{name: "trailing data after object", data: `{"a":1}{"b":2}`, wantErr: ErrJSONMalformed},
		{name: "top-level array rejected", data: `[1,2,3]`, wantErr: ErrJSONObjectExpected},
		{name: "top-level number rejected", data: `42`, wantErr: ErrJSONObjectExpected},
		{name: "top-level string rejected", data: `"hello"`, wantErr: ErrJSONObjectExpected},
		{name: "top-level duplicate key", data: `{"model":"m","model":"n"}`, wantDup: "model"},
		{name: "duplicate key in nested object", data: `{"meta":{"x":1,"x":2}}`, wantDup: "x"},
		{name: "duplicate key inside array element", data: `{"images":[{"image_url":"u","image_url":"v"}]}`, wantDup: "image_url"},
		{name: "escaped duplicate key collides with plain form", data: "{\"a\":1,\"\\u0061\":2}", wantDup: "a"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := AssertJSONObjectNoDuplicateKeys([]byte(tt.data))
			if tt.wantErr == nil && tt.wantDup == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			if tt.wantDup != "" {
				var dup *DuplicateJSONKeyError
				require.ErrorAs(t, err, &dup)
				assert.Equal(t, tt.wantDup, dup.Key)
				return
			}
			assert.ErrorIs(t, err, tt.wantErr)
		})
	}

	// A duplicate key is a distinct, addressable failure: it must surface as
	// DuplicateJSONKeyError, not the generic malformed sentinel, so callers can
	// report the offending field.
	t.Run("duplicate key is addressable not generic malformed", func(t *testing.T) {
		err := AssertJSONObjectNoDuplicateKeys([]byte(`{"a":1,"a":2}`))
		require.Error(t, err)
		var dup *DuplicateJSONKeyError
		require.ErrorAs(t, err, &dup)
		assert.Equal(t, "a", dup.Key)
	})
}
