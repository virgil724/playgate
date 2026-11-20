//go:build windows && cgo && libav

package wasapi

import (
	"math"
	"testing"
	"time"
)

// One second of a tone through the encoder gives one packet per frame, each
// lasting the configured frame length.
func TestOpusEncoderFrames(t *testing.T) {
	for _, ms := range []float64{2.5, 5, 10, 20} {
		e, err := newOpusEncoder(128000, ms)
		if err != nil {
			t.Fatal(err)
		}
		want := int(ms * 48)
		if e.frameSize != want {
			t.Errorf("%v ms: frame size %d, want %d", ms, e.frameSize, want)
		}
		var n int
		var total time.Duration
		blocks := 0
		c := Chunker{Frames: e.frameSize, OnBlock: func(b []float32) {
			blocks++
			defer func() {
				// libopus encodes synchronously: every frame's packet must
				// come out with that frame, not one frame later.
				if n != blocks {
					t.Errorf("%v ms: %d packets after %d frames", ms, n, blocks)
				}
			}()
			if err := e.encode(b, func(data []byte, d time.Duration) {
				if len(data) == 0 {
					t.Errorf("%v ms: empty packet", ms)
				}
				if d != time.Duration(ms*float64(time.Millisecond)) {
					t.Errorf("%v ms: packet of %v", ms, d)
				}
				n++
				total += d
			}); err != nil {
				t.Fatal(err)
			}
		}}
		tone := make([]float32, 48000)
		for i := range tone {
			tone[i] = float32(0.3 * math.Sin(2*math.Pi*440*float64(i)/48000))
		}
		c.Push(tone, len(tone), 1)
		e.free()
		if n != 48000/want {
			t.Errorf("%v ms: %d packets for %d frames", ms, n, 48000/want)
		}
	}
}
