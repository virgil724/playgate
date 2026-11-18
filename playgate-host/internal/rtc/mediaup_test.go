package rtc

import (
	"testing"

	"github.com/playgate/playgate-host/internal/core"
)

// TestWriteSampleBeforeConnected checks that a keyframe written before the peer
// connects is not counted as the viewer's first keyframe: it never reaches the
// viewer, so gating must keep waiting for the next one.
func TestWriteSampleBeforeConnected(t *testing.T) {
	p, err := NewPeer(Config{LoopbackOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.WriteSample(core.EncodedPacket{Data: minimalH264Keyframe(), IsKeyframe: true}, DefaultSampleDuration); err != nil {
		t.Fatal(err)
	}
	p.vmu.Lock()
	seen := p.seenKeyframe
	p.vmu.Unlock()
	if seen {
		t.Error("keyframe written before connect marked as seen")
	}
}
