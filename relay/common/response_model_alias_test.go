package common

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPatchJSONStringFieldRaw(t *testing.T) {
	t.Parallel()

	payload := []byte(`{"model":"upstream-model","nested":{"model":"inner"}}`)

	t.Run("rewrites field at path", func(t *testing.T) {
		patched, changed, err := PatchJSONStringFieldRaw(payload, "model", "caller-model")
		require.NoError(t, err)
		require.True(t, changed)
		require.JSONEq(t, `{"model":"caller-model","nested":{"model":"inner"}}`, string(patched),
			"only the target path should change")
	})

	t.Run("rewrites nested field only", func(t *testing.T) {
		patched, changed, err := PatchJSONStringFieldRaw(payload, "nested.model", "caller-model")
		require.NoError(t, err)
		require.True(t, changed)
		require.JSONEq(t, `{"model":"upstream-model","nested":{"model":"caller-model"}}`, string(patched))
	})

	t.Run("no-op when field absent", func(t *testing.T) {
		patched, changed, err := PatchJSONStringFieldRaw(payload, "modelVersion", "caller-model")
		require.NoError(t, err)
		require.False(t, changed)
		require.Equal(t, payload, patched)
	})

	t.Run("no-op when value already matches", func(t *testing.T) {
		patched, changed, err := PatchJSONStringFieldRaw(payload, "model", "upstream-model")
		require.NoError(t, err)
		require.False(t, changed)
		require.Equal(t, payload, patched)
	})

	t.Run("no-op when target value empty", func(t *testing.T) {
		patched, changed, err := PatchJSONStringFieldRaw(payload, "model", "")
		require.NoError(t, err)
		require.False(t, changed)
		require.Equal(t, payload, patched)
	})
}
