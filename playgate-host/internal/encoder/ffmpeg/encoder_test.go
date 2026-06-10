package ffmpeg

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/playgate/playgate-host/internal/core"
)

// fakeFFmpegEnv, when set, makes the test binary impersonate a short-lived
// ffmpeg: it exits with a non-zero status almost immediately, simulating the
// subprocess dying. This is fully deterministic and needs no real ffmpeg, cmd,
// or sh — just a re-exec of the test binary itself.
const fakeFFmpegEnv = "PLAYGATE_FAKE_FFMPEG"

// TestMain intercepts the re-exec: when invoked with fakeFFmpegEnv set, it acts
// as the fake ffmpeg and exits instead of running the test suite.
func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeFFmpegEnv); mode != "" {
		switch mode {
		case "die":
			// Pretend ffmpeg crashed before producing output.
			os.Exit(3)
		default:
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(testWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
}

// testWriter discards log output so tests stay quiet.
type testWriter struct{}

func (testWriter) Write(p []byte) (int, error) { return len(p), nil }

// fakeFFmpegPath returns the path to the current test binary, which re-execs as
// a fake ffmpeg when fakeFFmpegEnv is present in its environment.
func fakeFFmpegPath() string { return os.Args[0] }

func TestNewValidatesOptions(t *testing.T) {
	in := make(chan core.VideoFrame)
	if _, err := New(quietLogger(), Options{Width: 0, Height: 480, InputFormat: core.PixelFormatYUYV}, in); err == nil {
		t.Error("New should reject invalid options")
	}
	if _, err := New(quietLogger(), DefaultOptions(640, 480, 30, core.PixelFormatYUYV), in); err != nil {
		t.Errorf("New rejected valid options: %v", err)
	}
}

// TestEnqueueFrameDropOldest verifies the encoder's input backpressure: when the
// internal writer queue is full, enqueueing drops the oldest frame instead of
// blocking, so the caller (which reads from the upstream capture channel) is
// never stalled by a slow/stalled ffmpeg.
func TestEnqueueFrameDropOldest(t *testing.T) {
	e := &Encoder{log: quietLogger()}
	queue := make(chan core.VideoFrame, frameQueueSize) // cap == frameQueueSize (2)

	// Tag frames by Width so we can identify which survived.
	mk := func(id int) core.VideoFrame {
		return core.VideoFrame{Width: id, PixelFormat: core.PixelFormatYUYV}
	}

	// Fill the queue to capacity, then push extras. None of these calls may block.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 1; i <= frameQueueSize+5; i++ {
			e.enqueueFrame(queue, mk(i))
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("enqueueFrame blocked: drop-oldest not working")
	}

	// The queue must never exceed its capacity and should hold the newest frames.
	if len(queue) > frameQueueSize {
		t.Fatalf("queue length %d exceeds capacity %d", len(queue), frameQueueSize)
	}
	// Drain and confirm the surviving frames are recent (id > a few), proving old
	// frames were dropped rather than retained.
	var ids []int
	for len(queue) > 0 {
		ids = append(ids, (<-queue).Width)
	}
	if len(ids) == 0 {
		t.Fatal("queue unexpectedly empty")
	}
	for _, id := range ids {
		if id <= frameQueueSize {
			t.Errorf("stale frame %d survived; expected only recent frames %v", id, ids)
		}
	}
}

// TestSubprocessDeathReportedOnErrors points the encoder at a fake ffmpeg (a
// re-exec of the test binary) that exits non-zero immediately. Run must report
// the unexpected exit on Errors() and return nil (clean module shutdown), never
// panic.
func TestSubprocessDeathReportedOnErrors(t *testing.T) {
	t.Setenv(fakeFFmpegEnv, "die")

	in := make(chan core.VideoFrame)
	opts := DefaultOptions(320, 240, 30, core.PixelFormatYUYV)
	opts.FFmpegPath = fakeFFmpegPath()
	enc, err := New(quietLogger(), opts, in)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// The fake ffmpeg exits 3 almost immediately, before stdin EOF, which is the
	// "ffmpeg died unexpectedly" path: Run reports it on Errors() and returns nil.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runErrCh := make(chan error, 1)
	go func() { runErrCh <- enc.Run(ctx) }()

	// The subprocess (cmd /c exit 1 or sh -c exit 1) terminates almost
	// immediately. Run should report the exit on Errors() and return nil.
	select {
	case e := <-enc.Errors():
		if e == nil {
			t.Fatal("expected non-nil error on Errors() after subprocess death")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("timed out waiting for subprocess-death error")
	}

	select {
	case err := <-runErrCh:
		if err != nil {
			t.Errorf("Run should return nil on subprocess death, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Run did not return after subprocess death")
	}
}

// TestRunClosesChannelsOnExit verifies Run always closes its owned channels
// (packets, errs) when it returns, satisfying the core channel-ownership
// contract. We drive it with a short-lived fake subprocess and a cancellable
// context; whichever fires first, Run must return and close both channels.
func TestRunClosesChannelsOnExit(t *testing.T) {
	t.Setenv(fakeFFmpegEnv, "exit0")

	in := make(chan core.VideoFrame)
	opts := DefaultOptions(320, 240, 30, core.PixelFormatYUYV)
	opts.FFmpegPath = fakeFFmpegPath()
	enc, err := New(quietLogger(), opts, in)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- enc.Run(ctx) }()

	// Cancel shortly after start; the fake process likely already exited, but
	// either path must lead to a clean nil return.
	time.AfterFunc(200*time.Millisecond, cancel)

	select {
	case err := <-runErrCh:
		if err != nil {
			t.Errorf("Run should return nil on exit/cancel, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}

	// Both owned channels must be closed after Run returns. Drain then expect !ok.
	for {
		if _, ok := <-enc.Packets(); !ok {
			break
		}
	}
	for {
		if _, ok := <-enc.Errors(); !ok {
			break
		}
	}
}
