package host

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/playgate/playgate-host/internal/config"
	"github.com/playgate/playgate-host/internal/core"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- fakes ---------------------------------------------------------------

type fakeCapture struct {
	frames chan core.VideoFrame
	errs   chan error
	fps    int
}

func newFakeCapture(fps int) *fakeCapture {
	return &fakeCapture{
		frames: make(chan core.VideoFrame, 2),
		errs:   make(chan error, 1),
		fps:    fps,
	}
}

func (f *fakeCapture) Name() string                      { return "capture" }
func (f *fakeCapture) Frames() <-chan core.VideoFrame    { return f.frames }
func (f *fakeCapture) Errors() <-chan error              { return f.errs }
func (f *fakeCapture) Start(context.Context) error       { return nil }
func (f *fakeCapture) Run(ctx context.Context) error {
	defer close(f.frames)
	defer close(f.errs)
	t := time.NewTicker(time.Second / time.Duration(f.fps))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			frame := core.VideoFrame{
				Data:        []byte{0, 0, 0, 0},
				PixelFormat: core.PixelFormatYUYV,
				Width:       2, Height: 1,
				Timestamp: time.Now(),
			}
			select {
			case f.frames <- frame:
			default:
			}
		}
	}
}

// fakeEncoder emits a keyframe packet for every frame it reads.
type fakeEncoder struct {
	in      <-chan core.VideoFrame
	packets chan core.EncodedPacket
}

func newFakeEncoder(in <-chan core.VideoFrame) *fakeEncoder {
	return &fakeEncoder{in: in, packets: make(chan core.EncodedPacket, 4)}
}

func (e *fakeEncoder) Name() string                          { return "encoder" }
func (e *fakeEncoder) Packets() <-chan core.EncodedPacket    { return e.packets }
func (e *fakeEncoder) Run(ctx context.Context) error {
	defer close(e.packets)
	var pts time.Duration
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-e.in:
			if !ok {
				return nil
			}
			pkt := core.EncodedPacket{Data: []byte{0, 0, 0, 1, 0x65}, PTS: pts, IsKeyframe: true}
			pts += 33 * time.Millisecond
			select {
			case e.packets <- pkt:
			default:
			}
		}
	}
}

// fakeInput records the commands it receives.
type fakeInput struct {
	status chan core.TargetStatus
	mu     sync.Mutex
	got    []core.InputCommand
}

func newFakeInput() *fakeInput {
	return &fakeInput{status: make(chan core.TargetStatus, 4)}
}

func (i *fakeInput) Name() string                    { return "input" }
func (i *fakeInput) Status() <-chan core.TargetStatus { return i.status }
func (i *fakeInput) Start(context.Context) error     { return nil }
func (i *fakeInput) Send(cmd core.InputCommand) error {
	i.mu.Lock()
	i.got = append(i.got, cmd)
	i.mu.Unlock()
	return nil
}
func (i *fakeInput) count() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.got)
}
func (i *fakeInput) Run(ctx context.Context) error {
	defer close(i.status)
	<-ctx.Done()
	return nil
}

// --- tests ---------------------------------------------------------------

// TestRunShutsDownOnCancel verifies the whole wired pipeline starts then exits
// cleanly with no goroutine leak when the context is cancelled.
func TestRunShutsDownOnCancel(t *testing.T) {
	before := runtime.NumGoroutine()

	cap := newFakeCapture(60)
	enc := newFakeEncoder(cap.Frames())
	in := newFakeInput()
	deps := Deps{
		Capture: cap,
		Encoder: enc,
		Input:   in,
		Connect: func(ctx context.Context, _ *VideoRouter, _ *AudioRouter, _ InputSink) error {
			<-ctx.Done()
			return nil
		},
	}
	h := NewWithDeps(discardLogger(), config.Default(), deps, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error on clean shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return within deadline after cancel")
	}
	assertNoLeak(t, before)
}

// TestPipelineFlows verifies frames flow capture→encoder→router and that the
// router forwards packets to the registered sink.
func TestPipelineFlows(t *testing.T) {
	cap := newFakeCapture(120)
	enc := newFakeEncoder(cap.Frames())
	in := newFakeInput()

	sink := &countingSink{}
	deps := Deps{
		Capture: cap,
		Encoder: enc,
		Input:   in,
		Connect: func(ctx context.Context, router *VideoRouter, _ *AudioRouter, _ InputSink) error {
			router.SetSink(sink)
			<-ctx.Done()
			router.Clear()
			return nil
		},
	}
	h := NewWithDeps(discardLogger(), config.Default(), deps, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	<-done

	if sink.count() == 0 {
		t.Fatal("expected the router to forward at least one packet to the sink")
	}
}

type countingSink struct {
	mu sync.Mutex
	n  int
}

func (s *countingSink) WriteSample(_ core.EncodedPacket, _ time.Duration) error {
	s.mu.Lock()
	s.n++
	s.mu.Unlock()
	return nil
}
func (s *countingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

func assertNoLeak(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		runtime.GC()
		after := runtime.NumGoroutine()
		if after <= before+2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak: before=%d after=%d", before, after)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
