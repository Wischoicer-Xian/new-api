package constant

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsImageTaskPlatform(t *testing.T) {
	tests := []struct {
		name string
		p    TaskPlatform
		want bool
	}{
		{"wischoicer image platform", TaskPlatformWischoicerImage, true},
		{"suno legacy", TaskPlatformSuno, false},
		{"midjourney legacy", TaskPlatformMidjourney, false},
		{"video provider kling", TaskPlatform("kling"), false},
		{"empty platform", TaskPlatform(""), false},
		{"unknown platform", TaskPlatform("other"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsImageTaskPlatform(tt.p))
		})
	}
}
