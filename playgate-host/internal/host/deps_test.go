package host

import (
	"context"
	"testing"
	"time"

	"github.com/playgate/playgate-host/internal/config"
	"github.com/playgate/playgate-host/internal/core"
	"github.com/playgate/playgate-host/internal/metrics"
)

// TestLatencyTapRecordsAndForwards verifies the tap forwards frames to the
// encoder input and records capture-stage latency from the frame timestamp.
func TestLatencyTapRecordsAndForwards(t *testing.T) {
	mc := metrics.NewCollector()
	in := make(chan core.VideoFrame, 2)
	tap := newLatencyTap(discardLogger(), mc, in)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tap.run(ctx)

	// A frame captured 5ms ago.
	in <- core.VideoFrame{Timestamp: time.Now().Add(-5 * time.Millisecond)}

	select {
	case f, ok := <-tap.out():
		if !ok {
			t.Fatal("tap output closed unexpectedly")
		}
		_ = f
	case <-time.After(2 * time.Second):
		t.Fatal("tap did not forward the frame")
	}

	if tap.lastEnterTime().IsZero() {
		t.Error("tap did not record the enter time")
	}
	deadline := time.Now().Add(time.Second)
	for mc.Capture.Snapshot().Count == 0 {
		if time.Now().After(deadline) {
			t.Fatal("capture-stage latency not recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestBuildCaptureSynthetic verifies the synthetic backend is selected and
// produces a usable source on any OS.
func TestBuildCaptureSynthetic(t *testing.T) {
	cfg := config.Default()
	cfg.Capture.Source = config.CaptureSynthetic
	cfg.Capture.Width = 64
	cfg.Capture.Height = 48
	src, err := buildCapture(discardLogger(), cfg)
	if err != nil {
		t.Fatalf("buildCapture: %v", err)
	}
	if src.Name() != "capture" {
		t.Errorf("name = %q", src.Name())
	}
}

// TestBuildInputLog verifies the log target is selected in dev mode.
func TestBuildInputLog(t *testing.T) {
	cfg := config.Default()
	cfg.Input.Target = config.InputLog
	tgt, err := buildInput(discardLogger(), cfg, metrics.NewCollector())
	if err != nil {
		t.Fatalf("buildInput: %v", err)
	}
	if tgt.Name() != "input" {
		t.Errorf("name = %q", tgt.Name())
	}
}

// TestBuildDepsSyntheticLog ensures the full dev-mode dependency graph builds
// without a capture card / ffmpeg present (construction only; not Run).
func TestBuildDepsSyntheticLog(t *testing.T) {
	cfg := config.Default()
	cfg.Capture.Source = config.CaptureSynthetic
	cfg.Capture.Width = 320
	cfg.Capture.Height = 240
	cfg.Input.Target = config.InputLog

	deps, err := buildDeps(discardLogger(), cfg, metrics.NewCollector())
	if err != nil {
		t.Fatalf("buildDeps: %v", err)
	}
	if deps.Capture == nil || deps.Encoder == nil || deps.Input == nil || deps.Connect == nil {
		t.Fatal("buildDeps returned incomplete deps")
	}
	if deps.Session != nil {
		t.Error("session should be nil when gating disabled")
	}
}
