package doubao

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/QuantumNous/new-api/relay/common"
)

// TestConvertToRequestPayload_DurationPassthrough 锁定 doubao adapter duration 透传合同（WIS-462）：
// content-workstation 等只送 duration(int) 的 caller 也要把时长透传到 Seedance 上游，不被默认 5s。
// 镜像 gemini ResolveVeoDuration：Seconds（string）优先，空时回退 Duration（int）。
func TestConvertToRequestPayload_DurationPassthrough(t *testing.T) {
	cases := []struct {
		name     string
		seconds  string
		duration int
		wantDur  *int // nil = 不设上游 duration
		wantNote string
	}{
		{
			name:     "Seconds 空 + Duration>0 → 透传 Duration",
			seconds:  "",
			duration: 15,
			wantDur:  intPtr(15),
			wantNote: "content-workstation 送 duration=15（Seconds 空）必须透传到上游，不再被默认 5s",
		},
		{
			name:     "Seconds 优先于 Duration",
			seconds:  "10",
			duration: 15,
			wantDur:  intPtr(10),
			wantNote: "Seconds（string）与 Duration 并存时 Seconds 优先（gemini 同口径）",
		},
		{
			name:     "两者都空 → 不设上游 duration",
			seconds:  "",
			duration: 0,
			wantDur:  nil,
			wantNote: "都没给 → 不设 r.Duration（上游走自己的默认）",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &common.TaskSubmitReq{
				Model:    "doubao-seedance-2-0-260128",
				Seconds:  tc.seconds,
				Duration: tc.duration,
			}
			payload, err := (&TaskAdaptor{}).convertToRequestPayload(req)
			require.NoError(t, err, tc.wantNote)
			require.NotNil(t, payload, tc.wantNote)

			switch {
			case tc.wantDur == nil:
				assert.Nil(t, payload.Duration, "expected no upstream duration: %s", tc.wantNote)
			default:
				if assert.NotNil(t, payload.Duration, "expected upstream duration set: %s", tc.wantNote) {
					assert.Equal(t, *tc.wantDur, int(*payload.Duration), tc.wantNote)
				}
			}
		})
	}
}

func intPtr(v int) *int { return &v }
