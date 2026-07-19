package common

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMaskedModelNameIf(t *testing.T) {
	cases := []struct {
		name   string
		hidden bool
		model  string
		want   string
	}{
		{"hidden true masks real model", true, "claude-sonnet-4-5", MaskedSystemModelAlias},
		{"hidden false keeps real model", false, "claude-sonnet-4-5", "claude-sonnet-4-5"},
		{"hidden true empty name stays empty", true, "", ""},
		{"hidden false empty name stays empty", false, "", ""},
		{"hidden true masks another model", true, "gpt-4o", MaskedSystemModelAlias},
		{"hidden false keeps upstream-style name", false, "gemini-2.5-pro", "gemini-2.5-pro"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, MaskedModelNameIf(tc.hidden, tc.model))
		})
	}
}

// TestMaskedSystemModelAliasStable guards the alias contract: changing it would
// silently re-expose the hidden system key's real model name across every
// user-visible surface (logs, dashboard, perf metrics, task responses).
func TestMaskedSystemModelAliasStable(t *testing.T) {
	require.Equal(t, "知言云策系统调用", MaskedSystemModelAlias)
}
