package rtc

import (
	"testing"
	"time"
)

func TestSampleDuration(t *testing.T) {
	cases := []struct {
		name     string
		pts      time.Duration
		prev     time.Duration
		havePrev bool
		want     time.Duration
	}{
		{"first packet", 0, 0, false, DefaultSampleDuration},
		{"normal 33ms delta", 66 * time.Millisecond, 33 * time.Millisecond, true, 33 * time.Millisecond},
		{"zero delta", 50 * time.Millisecond, 50 * time.Millisecond, true, DefaultSampleDuration},
		{"negative delta (pts reset)", 5 * time.Millisecond, 100 * time.Millisecond, true, DefaultSampleDuration},
		{"large delta", time.Second, 0, true, time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SampleDuration(c.pts, c.prev, c.havePrev)
			if got != c.want {
				t.Errorf("SampleDuration = %v, want %v", got, c.want)
			}
		})
	}
}
