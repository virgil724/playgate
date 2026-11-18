package encoder

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/playgate/playgate-host/internal/core"
	"github.com/playgate/playgate-host/internal/encoder/ffmpeg"
)

func TestNewBackends(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := ffmpeg.DefaultOptions(320, 240, 30, core.PixelFormatYUYV)
	in := make(chan core.VideoFrame)

	enc, err := New("", log, opts, in)
	if err != nil {
		t.Fatalf("default backend: %v", err)
	}
	if c := enc.Caps(); c.LiveBitrate || c.CheapKeyframe {
		t.Errorf("cli caps = %+v, want zero", c)
	}
	if _, err := New("cli", log, opts, in); err != nil {
		t.Fatalf("cli backend: %v", err)
	}
	if _, err := New("bogus", log, opts, in); err == nil {
		t.Error("unknown backend should error")
	}
	if _, err := New("libav", log, opts, in); err == nil || !strings.Contains(err.Error(), "libav") {
		t.Errorf("libav in default build should error clearly, got %v", err)
	}
}
