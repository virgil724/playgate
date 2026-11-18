package host

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/playgate/playgate-host/internal/abr"
	"github.com/playgate/playgate-host/internal/config"
	"github.com/playgate/playgate-host/internal/core"
	"github.com/playgate/playgate-host/internal/rtc"
)

// liveBitrateCooldown / liveBitrateInterval are the ABR timings used when the
// encoder backend can retune a running encoder on the next frame (no restart).
const (
	liveBitrateCooldown = 500 * time.Millisecond
	liveBitrateInterval = 500 * time.Millisecond
)

// ABRSignal is one input to the ABR loop. Stats feeds the AIMD controller.
// TargetBps, when > 0, is a direct target from GCC (webrtc.gcc): the runner then
// follows it (clamped, smoothed) instead of running AIMD.
type ABRSignal struct {
	Stats     rtc.StatsSample
	TargetBps int
}

// SignalSource produces ABRSignals for the host-level ABR loop. The viewer
// aggregator implements it; a GCC-backed source can be added later.
type SignalSource interface {
	Signal() ABRSignal
}

// abrTimings returns the cooldown and sample interval for the given encoder
// capabilities. Live-bitrate backends change quickly (500ms); others keep the
// configured values since each change restarts the encoder.
func abrTimings(cfg config.Config, caps core.EncoderCaps) (cooldown, interval time.Duration) {
	cooldown = time.Duration(cfg.ABR.CooldownSeconds) * time.Second
	interval = time.Duration(cfg.ABR.SampleIntervalMS) * time.Millisecond
	if caps.LiveBitrate {
		cooldown = liveBitrateCooldown
		if interval <= 0 {
			interval = liveBitrateInterval
		}
	}
	if interval <= 0 {
		interval = 2 * time.Second
	}
	return cooldown, interval
}

// viewerStatsRegistry tracks connected viewers' stats sources and aggregates
// them into one sample. It implements SignalSource and StatsSource.
type viewerStatsRegistry struct {
	// controller returns the viewer id currently holding control ("" if none).
	controller func() string

	// gcc makes Signal() report a GCC-derived TargetBps.
	gcc bool

	mu      sync.Mutex
	viewers map[string]StatsSource
}

// targetSource is implemented by sources with a GCC estimate (*rtc.Peer).
type targetSource interface {
	TargetBitrate() (bps int, ok bool)
}

func newViewerStatsRegistry(controller func() string) *viewerStatsRegistry {
	return &viewerStatsRegistry{controller: controller, viewers: make(map[string]StatsSource)}
}

// Register adds a viewer; Unregister removes it.
func (r *viewerStatsRegistry) Register(id string, s StatsSource) {
	r.mu.Lock()
	r.viewers[id] = s
	r.mu.Unlock()
}

func (r *viewerStatsRegistry) Unregister(id string) {
	r.mu.Lock()
	delete(r.viewers, id)
	r.mu.Unlock()
}

// worseSample reports whether a is a worse link than b: congested beats
// uncongested, then higher loss, then higher RTT.
func worseSample(a, b rtc.StatsSample) bool {
	if a.Congested != b.Congested {
		return a.Congested
	}
	if a.LossFraction != b.LossFraction {
		return a.LossFraction > b.LossFraction
	}
	return a.RTT > b.RTT
}

// Stats aggregates: the controlling viewer's sample when it is registered and
// valid; otherwise the worst valid sample (congested first, then highest loss,
// then highest RTT). With no valid samples (including no viewers) it returns
// Valid=false, which makes the ABR loop hold the current bitrate.
func (r *viewerStatsRegistry) Stats() rtc.StatsSample {
	ctrlID := ""
	if r.controller != nil {
		ctrlID = r.controller()
	}
	r.mu.Lock()
	srcs := make(map[string]StatsSource, len(r.viewers))
	for id, s := range r.viewers {
		srcs[id] = s
	}
	r.mu.Unlock()

	if ctrlID != "" {
		if s, ok := srcs[ctrlID]; ok {
			if sm := s.Stats(); sm.Valid {
				return sm
			}
		}
	}
	var worst rtc.StatsSample
	for _, s := range srcs {
		sm := s.Stats()
		if !sm.Valid {
			continue
		}
		if !worst.Valid || worseSample(sm, worst) {
			worst = sm
		}
	}
	return worst
}

// Signal implements SignalSource. With GCC enabled TargetBps is the controlling
// viewer's estimate when available, else the minimum over viewers that have one
// (viewers without an estimate, e.g. WebCodecs transport, are ignored). With no
// estimates TargetBps is 0 and the runner falls back to AIMD on Stats.
func (r *viewerStatsRegistry) Signal() ABRSignal {
	sig := ABRSignal{Stats: r.Stats()}
	if r.gcc {
		sig.TargetBps = r.targetBps()
	}
	return sig
}

func (r *viewerStatsRegistry) targetBps() int {
	ctrlID := ""
	if r.controller != nil {
		ctrlID = r.controller()
	}
	r.mu.Lock()
	srcs := make(map[string]StatsSource, len(r.viewers))
	for id, s := range r.viewers {
		srcs[id] = s
	}
	r.mu.Unlock()
	est := func(s StatsSource) (int, bool) {
		ts, ok := s.(targetSource)
		if !ok {
			return 0, false
		}
		bps, ok := ts.TargetBitrate()
		return bps, ok && bps > 0
	}
	if s, ok := srcs[ctrlID]; ok && ctrlID != "" {
		if bps, ok := est(s); ok {
			return bps
		}
	}
	min := 0
	for _, s := range srcs {
		if bps, ok := est(s); ok && (min == 0 || bps < min) {
			min = bps
		}
	}
	return min
}

// gccBounds returns the initial/min/max bitrates handed to the GCC estimator.
func gccBounds(cfg config.Config) (initial, min, max int) {
	initial, min, max = cfg.Encoder.Bitrate, cfg.ABR.MinBitrate, cfg.ABR.MaxBitrate
	if min <= 0 {
		min = 500_000
	}
	if max <= 0 {
		max = initial
		if max < 8_000_000 {
			max = 8_000_000
		}
	}
	if initial <= 0 {
		initial = min
	}
	return initial, min, max
}

// startHostABR starts the single host-level ABR loop when cfg.ABR.Enabled and
// returns the registry viewers must register with; nil when ABR is disabled.
func startHostABR(ctx context.Context, log *slog.Logger, cfg config.Config, enc BitrateController, sink *inputSink) *viewerStatsRegistry {
	if !cfg.ABR.Enabled || enc == nil {
		return nil
	}
	var caps core.EncoderCaps
	if c, ok := enc.(interface{ Caps() core.EncoderCaps }); ok {
		caps = c.Caps()
	}
	var controller func() string
	if sink != nil && sink.manager != nil {
		controller = sink.manager.CurrentViewerID
	}
	reg := newViewerStatsRegistry(controller)
	reg.gcc = cfg.WebRTC.GCC
	cooldown, interval := abrTimings(cfg, caps)
	ctrl := abr.New(abr.Config{
		Min:           cfg.ABR.MinBitrate,
		Max:           cfg.ABR.MaxBitrate,
		Start:         cfg.Encoder.Bitrate,
		LossThreshold: cfg.ABR.LossThreshold,
		Cooldown:      cooldown,
	}, nil)
	r := newABRRunnerWith(log, reg, enc, ctrl, interval)
	r.signal = reg
	go r.run(ctx)
	return reg
}
