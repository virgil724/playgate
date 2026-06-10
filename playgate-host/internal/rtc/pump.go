package rtc

import (
	"context"
	"time"

	"github.com/playgate/playgate-host/internal/core"
)

// DefaultSampleDuration is used for the first packet (no previous PTS to diff
// against) and whenever the computed delta is non-positive (e.g. PTS reset).
// 1/30s matches the default capture FPS; it only affects RTP timestamp pacing,
// not decoding correctness.
const DefaultSampleDuration = time.Second / 30

// PumpPackets drives the video track from a channel of encoded packets until the
// channel is closed or ctx is cancelled. The per-sample duration is computed from
// the delta between consecutive packet PTS values (see SampleDuration).
//
// This is the glue the host module (T1's webrtc stub replacement) uses to connect
// the encoder's output to a Peer. It does not close the Peer; the caller owns the
// Peer lifecycle.
func (p *Peer) PumpPackets(ctx context.Context, packets <-chan core.EncodedPacket) error {
	var (
		havePrev bool
		prevPTS  time.Duration
	)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case pkt, ok := <-packets:
			if !ok {
				return nil
			}
			dur := SampleDuration(pkt.PTS, prevPTS, havePrev)
			prevPTS, havePrev = pkt.PTS, true
			if err := p.WriteSample(pkt, dur); err != nil {
				return err
			}
		}
	}
}

// SampleDuration computes the presentation duration for a packet given its PTS
// and the previous packet's PTS. For the first packet (havePrev false) or any
// non-positive delta, it returns DefaultSampleDuration.
func SampleDuration(pts, prevPTS time.Duration, havePrev bool) time.Duration {
	if !havePrev {
		return DefaultSampleDuration
	}
	d := pts - prevPTS
	if d <= 0 {
		return DefaultSampleDuration
	}
	return d
}
