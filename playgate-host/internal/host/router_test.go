package host

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/playgate/playgate-host/internal/audio/opus"
	"github.com/playgate/playgate-host/internal/core"
)

// recordingSink records the sample durations it is written, so tests can assert
// that every sink in the fan-out receives the same packets with the same
// (stream-level) duration.
type recordingSink struct {
	mu   sync.Mutex
	durs []time.Duration
}

func (s *recordingSink) WriteSample(_ core.EncodedPacket, d time.Duration) error {
	s.mu.Lock()
	s.durs = append(s.durs, d)
	s.mu.Unlock()
	return nil
}
func (s *recordingSink) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.durs)
}

// TestVideoRouterFanOut verifies the router broadcasts every packet to all
// registered sinks with an identical, stream-derived duration, and that a removed
// sink stops receiving while the rest keep going.
func TestVideoRouterFanOut(t *testing.T) {
	r := NewVideoRouter(discardLogger(), nil)
	a, b := &recordingSink{}, &recordingSink{}
	r.AddSink(a)
	r.AddSink(b)

	for _, pts := range []time.Duration{0, 40 * time.Millisecond, 80 * time.Millisecond} {
		r.forward(core.EncodedPacket{PTS: pts, IsKeyframe: pts == 0})
	}

	if a.len() != 3 || b.len() != 3 {
		t.Fatalf("fan-out count: a=%d b=%d, want 3 each", a.len(), b.len())
	}
	for i := range a.durs {
		if a.durs[i] != b.durs[i] {
			t.Errorf("duration[%d] differs between sinks: %v vs %v", i, a.durs[i], b.durs[i])
		}
	}
	if a.durs[1] != 40*time.Millisecond {
		t.Errorf("second sample duration = %v, want 40ms (PTS delta)", a.durs[1])
	}

	r.RemoveSink(a)
	r.forward(core.EncodedPacket{PTS: 120 * time.Millisecond})
	if a.len() != 3 {
		t.Errorf("removed sink kept receiving: a=%d, want 3", a.len())
	}
	if b.len() != 4 {
		t.Errorf("remaining sink stopped receiving: b=%d, want 4", b.len())
	}
}

type recordingAudioSink struct {
	mu sync.Mutex
	n  int
}

func (s *recordingAudioSink) WriteAudioSample(_ []byte, _ time.Duration) error {
	s.mu.Lock()
	s.n++
	s.mu.Unlock()
	return nil
}
func (s *recordingAudioSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// TestAudioRouterFanOut verifies Opus pages are broadcast to every audio sink.
func TestAudioRouterFanOut(t *testing.T) {
	r := NewAudioRouter(discardLogger())
	a, b := &recordingAudioSink{}, &recordingAudioSink{}
	r.AddSink(a)
	r.AddSink(b)

	ch := make(chan opus.Packet, 2)
	ch <- opus.Packet{Data: []byte{1}, Duration: 20 * time.Millisecond}
	ch <- opus.Packet{Data: []byte{2}, Duration: 20 * time.Millisecond}
	close(ch)
	r.Run(context.Background(), ch)

	if a.count() != 2 || b.count() != 2 {
		t.Fatalf("audio fan-out: a=%d b=%d, want 2 each", a.count(), b.count())
	}
}
