package gateway

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/playgate/playgate-host/internal/core"
)

// fakePort records written frames and serves reads from a pipe the test feeds.
type fakePort struct {
	r      *io.PipeReader
	w      *io.PipeWriter
	mu     sync.Mutex
	frames []sentFrame
	closed chan struct{}
	once   sync.Once
}

type sentFrame struct {
	at      time.Time
	buttons uint32
	ly      int16
	seq     uint32
}

func newFakePort() *fakePort {
	r, w := io.Pipe()
	return &fakePort{r: r, w: w, closed: make(chan struct{})}
}

func (p *fakePort) Read(b []byte) (int, error) { return p.r.Read(b) }

func (p *fakePort) Write(b []byte) (int, error) {
	if b[len(b)-1] != 0 {
		panic("frame without delimiter")
	}
	_, pl, err := DecodeFrame(b[:len(b)-1])
	if err != nil {
		panic(err)
	}
	p.mu.Lock()
	p.frames = append(p.frames, sentFrame{
		at:      time.Now(),
		buttons: binary.LittleEndian.Uint32(pl[1:]),
		ly:      int16(binary.LittleEndian.Uint16(pl[7:])),
		seq:     binary.LittleEndian.Uint32(pl[13:]),
	})
	p.mu.Unlock()
	return len(b), nil
}

func (p *fakePort) Close() error {
	p.once.Do(func() {
		_ = p.r.Close()
		close(p.closed)
	})
	return nil
}

func (p *fakePort) sent() []sentFrame {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]sentFrame(nil), p.frames...)
}

func statusFrame(hid uint8, lastSeq uint32, us uint16) []byte {
	pl := make([]byte, statusSize)
	pl[0], pl[1], pl[2] = statusVersion, hid, 1
	binary.LittleEndian.PutUint32(pl[4:], lastSeq)
	binary.LittleEndian.PutUint16(pl[18:], us)
	return encodeFrame(typeStatus, pl)
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func startTarget(t *testing.T, open OpenFunc, opts ...Option) (*Target, context.CancelFunc, chan struct{}) {
	t.Helper()
	tg := New(quietLog(), "fake", append([]Option{WithOpenFunc(open), WithRateHz(0)}, opts...)...)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = tg.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return tg, cancel, done
}

// A held button is re-sent every keepalive with a growing sequence, and a
// release is sent once and never repeated (the old idle keepalive was 1 s).
func TestHeldStateKeepaliveAndSequence(t *testing.T) {
	port := newFakePort()
	tg, _, _ := startTarget(t, func(context.Context) (io.ReadWriteCloser, error) { return port, nil },
		WithKeepalive(20*time.Millisecond))

	_ = tg.Send(core.InputCommand{Buttons: core.ButtonA, LY: -1, Timestamp: time.Now()})
	time.Sleep(150 * time.Millisecond)
	_ = tg.Send(core.InputCommand{})
	time.Sleep(1200 * time.Millisecond)

	frames := port.sent()
	var held, after int
	released := false
	for i, f := range frames {
		if i > 0 && f.seq != frames[i-1].seq+1 {
			t.Fatalf("seq not consecutive at %d: %d after %d", i, f.seq, frames[i-1].seq)
		}
		switch {
		case f.buttons == core.ButtonA:
			if f.ly != -axisMax {
				t.Fatalf("ly = %d, want %d", f.ly, -axisMax)
			}
			held++
		case held > 0 && f.buttons == 0:
			if released {
				after++
			}
			released = true
		}
	}
	if held < 5 {
		t.Errorf("held state written %d times in 150ms with a 20ms keepalive, want >= 5", held)
	}
	if !released || after > 0 {
		t.Errorf("release: seen %v, extra neutral frames %d, want none", released, after)
	}
}

// Status reports drive TargetStatus, and a read failure reopens the port and
// re-sends the held state.
func TestStatusAndReconnect(t *testing.T) {
	var mu sync.Mutex
	var ports []*fakePort
	open := func(context.Context) (io.ReadWriteCloser, error) {
		mu.Lock()
		defer mu.Unlock()
		p := newFakePort()
		ports = append(ports, p)
		return p, nil
	}
	cur := func(i int) *fakePort {
		mu.Lock()
		defer mu.Unlock()
		if i < len(ports) {
			return ports[i]
		}
		return nil
	}
	tg, _, _ := startTarget(t, open)

	waitFor(t, "first open", func() bool { return cur(0) != nil })
	_ = tg.Send(core.InputCommand{Buttons: core.ButtonB})
	go func() { _, _ = cur(0).w.Write(statusFrame(HIDConnected, 1, 5800)) }()

	got := map[core.TargetStatus]bool{}
	waitFor(t, "Connected status", func() bool {
		select {
		case s := <-tg.Status():
			got[s] = true
		default:
		}
		return got[core.TargetStatusConnected]
	})

	_ = cur(0).w.CloseWithError(errors.New("cable pulled"))
	waitFor(t, "reopen", func() bool { return cur(1) != nil })
	waitFor(t, "held state re-sent after reopen", func() bool {
		for _, f := range cur(1).sent() {
			if f.buttons == core.ButtonB {
				return true
			}
		}
		return false
	})
	select {
	case <-cur(0).closed:
	default:
		t.Error("old port not closed")
	}
}

func TestOpenFailureRetries(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	open := func(context.Context) (io.ReadWriteCloser, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return nil, errors.New("no such port")
	}
	_, cancel, done := startTarget(t, open)
	waitFor(t, "retry", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls >= 2
	})
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
