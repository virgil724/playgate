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

// VideoRouter consumes the encoder's packet stream once and fans each packet out
// to every currently-registered viewer sink (mesh broadcast). When no viewer is
// connected the packets are discarded (the encoder keeps running so a new viewer
// gets video immediately). It also records the rtc-stage latency per packet.
//
// Sink registration is concurrency-safe: the connection manager calls AddSink
// when a viewer connects and RemoveSink when it disconnects. Each sink (an
// rtc.Peer) does its own keyframe gating, so a late joiner simply waits for the
// next keyframe before its video starts.
type VideoRouter struct {
	log     *slog.Logger
	metrics *metrics.Collector

	mu       sync.Mutex
	sinks    map[PacketSink]struct{}
	prevPTS  time.Duration
	havePrev bool
}

// NewVideoRouter constructs a router. metrics may be nil.
func NewVideoRouter(log *slog.Logger, mc *metrics.Collector) *VideoRouter {
	if log == nil {
		log = slog.Default()
	}
	return &VideoRouter{
		log:     log.With("module", "video-router"),
		metrics: mc,
		sinks:   make(map[PacketSink]struct{}),
	}
}

// AddSink registers a viewer sink to receive the broadcast.
func (r *VideoRouter) AddSink(s PacketSink) {
	r.mu.Lock()
	r.sinks[s] = struct{}{}
	r.mu.Unlock()
}

// RemoveSink unregisters a viewer sink (that viewer disconnected).
func (r *VideoRouter) RemoveSink(s PacketSink) {
	r.mu.Lock()
	delete(r.sinks, s)
	r.mu.Unlock()
}

// Run drains packets from the encoder until the channel closes or ctx is
// cancelled, forwarding each to all active sinks.
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

// forward writes one packet to every active sink with a computed sample duration
// and records the rtc-stage write latency. The duration is a property of the
// stream (PTS delta), computed once and shared by all sinks.
func (r *VideoRouter) forward(pkt core.EncodedPacket) {
	r.mu.Lock()
	dur := sampleDuration(pkt.PTS, r.prevPTS, r.havePrev)
	r.prevPTS, r.havePrev = pkt.PTS, true
	sinks := make([]PacketSink, 0, len(r.sinks))
	for s := range r.sinks {
		sinks = append(sinks, s)
	}
	r.mu.Unlock()

	if len(sinks) == 0 {
		return
	}
	start := time.Now()
	for _, sink := range sinks {
		if err := sink.WriteSample(pkt, dur); err != nil {
			r.log.Debug("write sample failed", "err", err)
		}
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

// AudioRouter consumes the audio source's Opus page stream once and fans each
// page out to every registered viewer sink, discarding pages when no viewer is
// connected. It is the audio analogue of VideoRouter and is created only when
// audio capture is enabled.
type AudioRouter struct {
	log *slog.Logger

	mu    sync.Mutex
	sinks map[AudioSink]struct{}
}

// NewAudioRouter constructs an audio router.
func NewAudioRouter(log *slog.Logger) *AudioRouter {
	if log == nil {
		log = slog.Default()
	}
	return &AudioRouter{
		log:   log.With("module", "audio-router"),
		sinks: make(map[AudioSink]struct{}),
	}
}

// AddSink registers a viewer sink to receive the audio broadcast.
func (r *AudioRouter) AddSink(s AudioSink) {
	r.mu.Lock()
	r.sinks[s] = struct{}{}
	r.mu.Unlock()
}

// RemoveSink unregisters a viewer sink (that viewer disconnected).
func (r *AudioRouter) RemoveSink(s AudioSink) {
	r.mu.Lock()
	delete(r.sinks, s)
	r.mu.Unlock()
}

// Run drains Opus pages until the channel closes or ctx is cancelled, forwarding
// each to all active sinks.
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
			sinks := make([]AudioSink, 0, len(r.sinks))
			for s := range r.sinks {
				sinks = append(sinks, s)
			}
			r.mu.Unlock()
			for _, sink := range sinks {
				if err := sink.WriteAudioSample(pkt.Data, pkt.Duration); err != nil {
					r.log.Debug("write audio sample failed", "err", err)
				}
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
