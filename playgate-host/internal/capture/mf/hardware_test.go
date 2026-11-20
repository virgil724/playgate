//go:build windows

package mf

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"
)

// TestHardwareCapture captures two seconds from a real device. Set
// PLAYGATE_MF_DEVICE, e.g. "Live Gamer Ultra-Video".
func TestHardwareCapture(t *testing.T) {
	dev := os.Getenv("PLAYGATE_MF_DEVICE")
	if dev == "" {
		t.Skip("PLAYGATE_MF_DEVICE not set")
	}
	s, err := New(slog.Default(), Config{Device: dev, Width: 1280, Height: 720, FPS: 60})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	go s.Run(ctx)
	n := 0
	for f := range s.Frames() {
		if len(f.Data) != 1280*720*3/2 || f.Width != 1280 || f.Height != 720 {
			t.Fatalf("frame %dx%d with %d bytes", f.Width, f.Height, len(f.Data))
		}
		n++
	}
	if n < 90 {
		t.Errorf("%d frames in 2 s at 60 fps", n)
	}
	t.Logf("%d frames", n)
}
