package testsrc

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestSmokeFFmpeg runs the real ffmpeg when available; skipped otherwise.
func TestSmokeFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	s, err := New(nil, Config{Width: 320, Height: 240, FPS: 30, TextFile: filepath.Join(t.TempDir(), "o.txt")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	select {
	case f := <-s.Frames():
		if len(f.Data) != 320*240*3/2 {
			t.Fatalf("len %d", len(f.Data))
		}
	case err := <-done:
		t.Fatalf("run exited: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("no frame")
	}
	cancel()
	<-done
}
