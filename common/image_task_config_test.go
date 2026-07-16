package common

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseMaxImageTasksPerUser pins the §6.1/§12.1/§17 mandatory-cap
// invariant: the env value must be a positive integer. Empty falls back to the
// dormant provisional default; 0, negative, and non-numeric are illegal and
// fail startup (fail-closed) so the cap can never be silently disabled. Parse
// does not consult the create-enablement switch, so an illegal cap fails
// regardless of whether create is live.
func TestParseMaxImageTasksPerUser(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    int
		wantErr bool
	}{
		{name: "unset falls back to dormant default", raw: "", want: constant.DefaultMaxImageTasksPerUser},
		{name: "positive accepted", raw: "5", want: 5},
		{name: "large positive accepted", raw: "1000", want: 1000},
		{name: "zero rejected", raw: "0", wantErr: true},
		{name: "negative rejected", raw: "-1", wantErr: true},
		{name: "non-numeric rejected", raw: "abc", wantErr: true},
		{name: "float rejected", raw: "3.5", wantErr: true},
		{name: "whitespace rejected", raw: "  ", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseMaxImageTasksPerUser(tt.raw)
			if tt.wantErr {
				require.Error(t, err, "illegal cap must fail startup (fail-closed), not be treated as unlimited")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	// The dormant default must equal the documented provisional value, not zero,
	// so a deployed instance with an unset env still has a positive cap.
	assert.Equal(t, 10, constant.DefaultMaxImageTasksPerUser)
}
