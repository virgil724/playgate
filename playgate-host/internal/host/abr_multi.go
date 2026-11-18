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

// ABRSignal is one input to the ABR loop. Today only Stats is consumed (AIMD).
// TargetBps is reserved for a direct-target source such as GCC (T9): when > 0 a
// source is saying "use this bitrate" instead of providing loss/RTT. It is
// deliberately not consumed yet.
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

	mu      sync.Mutex
	viewers map[string]StatsSource
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

// Stats aggregates: the controlling viewer's sample when it is registered and
// valid; otherwise the worst valid sample (highest loss, then highest RTT).
// With no valid samples (including no viewers) it returns Valid=false, which
// makes the ABR loop hold the current bitrate.
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
		if !worst.Valid || sm.LossFraction > worst.LossFraction ||
			(sm.LossFraction == worst.LossFraction && sm.RTT > worst.RTT) {
			worst = sm
		}
	}
	return worst
}

// Signal implements SignalSource.
func (r *viewerStatsRegistry) Signal() ABRSignal { return ABRSignal{Stats: r.Stats()} }

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
