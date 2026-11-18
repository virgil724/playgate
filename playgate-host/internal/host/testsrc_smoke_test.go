package host

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/playgate/playgate-host/internal/config"
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
