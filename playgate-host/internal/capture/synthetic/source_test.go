package synthetic

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/playgate/playgate-host/internal/core"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestValidate(t *testing.T) {
	cases := []struct {
		cfg Config
		ok  bool
	}{
		{Config{Width: 320, Height: 240, FPS: 30}, true},
		{Config{Width: 0, Height: 240, FPS: 30}, false},
		{Config{Width: 321, Height: 240, FPS: 30}, false}, // odd width
		{Config{Width: 320, Height: 240, FPS: 0}, false},
	}
	for i, c := range cases {
		err := c.cfg.Validate()
		if (err == nil) != c.ok {
			t.Errorf("case %d: ok=%v err=%v", i, c.ok, err)
		}
	}
}

func TestProducesYUYVFrames(t *testing.T) {
	src, err := New(testLogger(), Config{Width: 64, Height: 48, FPS: 200})
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = src.Run(ctx); close(done) }()

	select {
	case frame, ok := <-src.Frames():
		if !ok {
			t.Fatal("frames channel closed before a frame arrived")
		}
		if frame.PixelFormat != core.PixelFormatYUYV {
			t.Errorf("format = %v, want YUYV", frame.PixelFormat)
		}
		if frame.Width != 64 || frame.Height != 48 {
			t.Errorf("geometry = %dx%d", frame.Width, frame.Height)
		}
		wantLen := 64 * 48 * 2 // YUYV = 2 bytes/pixel
		if len(frame.Data) != wantLen {
			t.Errorf("data len = %d, want %d", len(frame.Data), wantLen)
		}
		if frame.Timestamp.IsZero() {
			t.Error("frame timestamp not set")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no frame within deadline")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	// After Run returns, the frames channel must be closed (channel contract).
	for range src.Frames() {
	}
}
