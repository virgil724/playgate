//go:build windows && cgo && libav

package wasapi

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"
)

// TestHardwareCapture runs the source on a real endpoint for two seconds.
// Set PLAYGATE_WASAPI_DEVICE, e.g. "HDMI (Live Gamer Ultra-Audio)", and
// PLAYGATE_WASAPI_EXCLUSIVE=1 to try exclusive mode.
func TestHardwareCapture(t *testing.T) {
	dev := os.Getenv("PLAYGATE_WASAPI_DEVICE")
	if dev == "" {
		t.Skip("PLAYGATE_WASAPI_DEVICE not set")
	}
	s, err := New(slog.Default(), Config{Device: dev, Exclusive: os.Getenv("PLAYGATE_WASAPI_EXCLUSIVE") == "1"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	var n int
	var total time.Duration
	for p := range s.Packets() {
		n++
		total += p.Duration
	}
	<-done
	// Two seconds of 10 ms frames, less start-up.
	if n < 150 || total < 1500*time.Millisecond {
		t.Errorf("%d packets, %v of audio in 2 s", n, total)
	}
	t.Logf("%d packets, %v of audio", n, total)
}
