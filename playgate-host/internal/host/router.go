package host

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/playgate/playgate-host/internal/audio/opus"
	"github.com/playgate/playgate-host/internal/core"
	"github.com/playgate/playgate-host/internal/metrics"
)

// PacketSink is the subset of *rtc.Peer the router writes video to. The active
// viewer connection registers itself as the sink; the router forwards every
// encoded packet to it.
type PacketSink interface {
	WriteSample(pkt core.EncodedPacket, duration time.Duration) error
}

// VideoRouter consumes the encoder's packet stream once and forwards each packet
// to the currently-registered viewer sink. When no viewer is connected the
// packets are discarded (the encoder keeps running so a new viewer gets video
// immediately). It also records the rtc-stage latency for each forwarded packet.
//
// Swapping sinks is concurrency-safe: the connection manager calls SetSink when a
// viewer connects and Clear when it disconnects.
type VideoRouter struct {
	log     *slog.Logger
	metrics *metrics.Collector

	mu       sync.Mutex
	sink     PacketSink
	prevPTS  time.Duration
	havePrev bool
}

// NewVideoRouter constructs a router. metrics may be nil.
func NewVideoRouter(log *slog.Logger, mc *metrics.Collector) *VideoRouter {
	if log == nil {
		log = slog.Default()
	}
	return &VideoRouter{log: log.With("module", "video-router"), metrics: mc}
}

// SetSink registers the active viewer sink. It resets the PTS delta tracking so
// the new connection's first sample uses the default duration.
func (r *VideoRouter) SetSink(s PacketSink) {
	r.mu.Lock()
	r.sink = s
	r.havePrev = false
	r.prevPTS = 0
	r.mu.Unlock()
}

// Clear removes the active sink (viewer disconnected).
func (r *VideoRouter) Clear() {
	r.mu.Lock()
	r.sink = nil
	r.mu.Unlock()
}

// Run drains packets from the encoder until the channel closes or ctx is
// cancelled, forwarding each to the active sink (if any).
func (r *VideoRouter) Run(ctx context.Context, packets <-chan core.EncodedPacket) {
	for {
		select {
		case <-ctx.Done():
			return
		case pkt, ok := <-packets:
			if !ok {
				return
			}
			r.forward(pkt)
		}
	}
}

// forward writes one packet to the active sink with a computed sample duration
// and records the rtc-stage write latency.
func (r *VideoRouter) forward(pkt core.EncodedPacket) {
	r.mu.Lock()
	sink := r.sink
	dur := sampleDuration(pkt.PTS, r.prevPTS, r.havePrev)
	r.prevPTS, r.havePrev = pkt.PTS, true
	r.mu.Unlock()

	if sink == nil {
		return
	}
	start := time.Now()
	if err := sink.WriteSample(pkt, dur); err != nil {
		r.log.Debug("write sample failed", "err", err)
		return
	}
	if r.metrics != nil {
		r.metrics.RTC.Observe(time.Since(start))
	}
}

// AudioSink is the subset of *rtc.Peer the audio router writes to. The active
// viewer connection registers itself as the sink; the router forwards every Opus
// page to it.
type AudioSink interface {
	WriteAudioSample(data []byte, duration time.Duration) error
}

// AudioRouter consumes the audio source's Opus page stream once and forwards each
// page to the currently-registered viewer sink, discarding pages when no viewer
// is connected. It is the audio analogue of VideoRouter and is created only when
// audio capture is enabled.
type AudioRouter struct {
	log *slog.Logger

	mu   sync.Mutex
	sink AudioSink
}

// NewAudioRouter constructs an audio router.
func NewAudioRouter(log *slog.Logger) *AudioRouter {
	if log == nil {
		log = slog.Default()
	}
	return &AudioRouter{log: log.With("module", "audio-router")}
}

// SetSink registers the active viewer sink.
func (r *AudioRouter) SetSink(s AudioSink) {
	r.mu.Lock()
	r.sink = s
	r.mu.Unlock()
}

// Clear removes the active sink (viewer disconnected).
func (r *AudioRouter) Clear() {
	r.mu.Lock()
	r.sink = nil
	r.mu.Unlock()
}

// Run drains Opus pages until the channel closes or ctx is cancelled, forwarding
// each to the active sink (if any).
func (r *AudioRouter) Run(ctx context.Context, packets <-chan opus.Packet) {
	for {
		select {
		case <-ctx.Done():
			return
		case pkt, ok := <-packets:
			if !ok {
				return
			}
			r.mu.Lock()
			sink := r.sink
			r.mu.Unlock()
			if sink == nil {
				continue
			}
			if err := sink.WriteAudioSample(pkt.Data, pkt.Duration); err != nil {
				r.log.Debug("write audio sample failed", "err", err)
			}
		}
	}
}

// defaultSampleDuration mirrors rtc.DefaultSampleDuration (1/30s) without
// importing the rtc package, so the router stays test-friendly.
const defaultSampleDuration = time.Second / 30

// sampleDuration computes the presentation duration for a packet from its PTS
// delta, falling back to the default for the first packet or non-positive deltas.
func sampleDuration(pts, prevPTS time.Duration, havePrev bool) time.Duration {
	if !havePrev {
		return defaultSampleDuration
	}
	d := pts - prevPTS
	if d <= 0 {
		return defaultSampleDuration
	}
	return d
}
