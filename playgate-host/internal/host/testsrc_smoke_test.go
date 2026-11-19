package host

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/playgate/playgate-host/internal/config"
	"github.com/playgate/playgate-host/internal/core"
	"github.com/playgate/playgate-host/internal/metrics"
)

// TestTestSrcEncodesWithDefaultFormat runs the real testsrc capture and the
// real ffmpeg CLI encoder with the default capture.format (YUYV) and checks a
// keyframe comes out. testsrc emits NV12; before encoderInputFormat the encoder
// was configured for YUYV and dropped every frame. Skips without ffmpeg.
func TestTestSrcEncodesWithDefaultFormat(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	cfg := config.Default()
	cfg.Capture.Source = config.CaptureTestSrc
	cfg.Capture.Width, cfg.Capture.Height, cfg.Capture.FPS = 320, 240, 30
	cfg.Input.Target = config.InputLog
	cfg.Overlay.TextFile = filepath.Join(t.TempDir(), "overlay.txt")

	deps, err := buildDeps(discardLogger(), cfg, metrics.NewCollector())
	if err != nil {
		t.Fatalf("buildDeps: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := deps.Capture.Start(ctx); err != nil {
		t.Fatalf("capture start: %v", err)
	}
	go func() { _ = deps.Capture.Run(ctx) }()
	go func() { _ = deps.Encoder.Run(ctx) }()

	packets := 0
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("no keyframe within timeout (%d packets)", packets)
		case pkt, ok := <-deps.Encoder.Packets():
			if !ok {
				t.Fatalf("encoder closed after %d packets without a keyframe", packets)
			}
			packets++
			if pkt.IsKeyframe && len(pkt.Data) > 0 {
				return
			}
		}
	}
}

// TestTestSrcSurvivesOverlayUpdates runs real ffmpeg drawtext (reload=1)
// against the overlay target while controller input changes constantly.
// drawtext re-opens the file every frame and exits on a failed or empty read;
// the old rename-based write made it exit with "Permission denied" on Windows.
// Skips without ffmpeg.
func TestTestSrcSurvivesOverlayUpdates(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	cfg := config.Default()
	cfg.Capture.Source = config.CaptureTestSrc
	cfg.Capture.Width, cfg.Capture.Height, cfg.Capture.FPS = 320, 240, 120
	cfg.Input.Target = config.InputOverlay
	cfg.Overlay.TextFile = filepath.Join(t.TempDir(), "overlay.txt")

	deps, err := buildDeps(discardLogger(), cfg, metrics.NewCollector())
	if err != nil {
		t.Fatalf("buildDeps: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := deps.Input.Start(ctx); err != nil {
		t.Fatalf("overlay start: %v", err)
	}
	go func() { _ = deps.Input.Run(ctx) }()
	if err := deps.Capture.Start(ctx); err != nil {
		t.Fatalf("capture start: %v", err)
	}
	capErr := make(chan error, 1)
	go func() { capErr <- deps.Capture.Run(ctx) }()

	// Change the input every 2 ms for 5 s at 120 fps (the old rename-based
	// write failed here in under a second); the target flushes on its own
	// interval while ffmpeg keeps re-reading the file.
	stopSend := make(chan struct{})
	defer close(stopSend)
	go func() {
		for i := uint32(0); ; i++ {
			select {
			case <-stopSend:
				return
			case <-time.After(2 * time.Millisecond):
				_ = deps.Input.Send(core.InputCommand{Seq: i, Buttons: i})
			}
		}
	}()

	frames := 0
	deadline := time.After(5 * time.Second)
	for {
		select {
		case err := <-capErr:
			t.Fatalf("testsrc capture exited after %d frames: %v", frames, err)
		case _, ok := <-deps.Capture.Frames():
			if !ok {
				t.Fatalf("frames channel closed after %d frames", frames)
			}
			frames++
		case <-deadline:
			if frames < 60 {
				t.Fatalf("only %d frames in 5 s", frames)
			}
			return
		}
	}
}
