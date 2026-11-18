package ffmpeg

import (
	"context"
	"log/slog"
	"time"

	"github.com/playgate/playgate-host/internal/core"
)

// PacketEmitter turns encoded access units into core.EncodedPackets with the
// semantics every encoder backend shares: PTS derived from capture timestamps,
// and a non-blocking, keyframe-preserving hand-off to the output channel. It is
// not safe for concurrent use; one goroutine (the encoder's reader) drives it.
//
// PTS comes from the frame's capture time, NOT from the time the packet is read
// (an encoder can flush several at once, which would collapse into duplicate RTP
// timestamps) and NOT from a frame counter (which falls behind real time
// whenever a frame is dropped or the encoder restarts, and runs at the wrong
// speed if the device delivers a different rate than configured).
type PacketEmitter struct {
	log *slog.Logger
	fps int

	// ptsBase is the capture time PTS is measured from.
	ptsBase time.Time
	lastPTS time.Duration
	havePTS bool

	// waitKey is set once an inter frame has been dropped: every later inter
	// frame references it, so they are dropped too until the next keyframe.
	waitKey bool
}

// NewPacketEmitter returns an emitter for a stream of nominal frame rate fps
// (used to advance PTS when frames carry no capture time; <=0 means 30).
func NewPacketEmitter(log *slog.Logger, fps int) *PacketEmitter {
	if log == nil {
		log = slog.Default()
	}
	return &PacketEmitter{log: log, fps: fps}
}

// NextPTS returns the PTS for the access unit produced from the frame captured
// at capture: its capture time relative to the first frame's. Without a capture
// time it advances one nominal frame interval. The result is strictly
// increasing.
func (p *PacketEmitter) NextPTS(capture time.Time) time.Duration {
	fps := p.fps
	if fps <= 0 {
		fps = 30
	}
	pts := p.lastPTS + time.Second/time.Duration(fps)
	if !capture.IsZero() {
		if p.ptsBase.IsZero() {
			p.ptsBase = capture
		}
		pts = capture.Sub(p.ptsBase)
	} else if !p.havePTS {
		pts = 0
	}
	if p.havePTS && pts <= p.lastPTS {
		pts = p.lastPTS + time.Millisecond
	}
	p.lastPTS, p.havePTS = pts, true
	return pts
}

// Emit converts an access unit to a core.EncodedPacket and pushes it onto out
// without ever blocking the encoder on a slow consumer. capture is the source
// frame's capture time and written is when it entered the encoder (either may be
// zero).
//
// When the channel is full an inter frame is dropped — and so is every inter
// frame after it until the next keyframe, because each one references the frame
// before it and decoding past a gap shows corruption rather than a clean pause.
// A keyframe is never dropped: it replaces whatever is queued, since it makes
// everything older than itself unnecessary.
func (p *PacketEmitter) Emit(ctx context.Context, out chan core.EncodedPacket, data []byte, isKeyframe bool, capture, written time.Time) {
	pkt := core.EncodedPacket{
		Data:        data,
		PTS:         p.NextPTS(capture),
		IsKeyframe:  isKeyframe,
		EncodeStart: written,
	}
	if p.waitKey {
		if !pkt.IsKeyframe {
			return
		}
		p.waitKey = false
	}
	select {
	case out <- pkt:
		return
	default:
	}
	if !pkt.IsKeyframe {
		p.waitKey = true
		p.log.Debug("dropped encoded packet: consumer not keeping up; waiting for keyframe")
		return
	}
	for drained := false; !drained; {
		select {
		case <-out:
		default:
			drained = true
		}
	}
	select {
	case out <- pkt:
	case <-ctx.Done():
	default:
		p.waitKey = true
	}
}
