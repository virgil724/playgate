package host

import (
	"context"
	"log/slog"
	"time"

	"github.com/playgate/playgate-host/internal/abr"
	"github.com/playgate/playgate-host/internal/config"
	"github.com/playgate/playgate-host/internal/core"
	"github.com/playgate/playgate-host/internal/rtc"
)

// StatsSource provides periodic WebRTC transport statistics for the ABR loop.
// *rtc.Peer implements it via Stats(); tests inject a fake sequence.
type StatsSource interface {
	Stats() rtc.StatsSample
}

// BitrateController receives ABR bitrate decisions. The encoder wrapper
// implements it via SetBitrate (which restarts the ffmpeg subprocess).
type BitrateController interface {
	SetBitrate(bps int)
}

// abrRunner ties a stats source, the AIMD controller, and a bitrate controller
// together. It is constructed per viewer connection (so a new viewer starts from
// the configured starting bitrate) and runs until ctx is cancelled.
type abrRunner struct {
	log      *slog.Logger
	stats    StatsSource
	enc      BitrateController
	ctrl     *abr.Controller
	interval time.Duration
	// signal, when non-nil, replaces stats as the loop input (see SignalSource).
	signal SignalSource

	// GCC-mode state (used when a signal carries TargetBps > 0).
	now         func() time.Time
	gccSmoothed float64   // EWMA of clamped GCC target
	gccApplied  int       // last bitrate pushed to the encoder (0 = Start)
	gccLast     time.Time // time of last push, for cooldown
}

const (
	gccEWMAAlpha  = 0.3  // weight of the newest GCC sample
	gccMinChange  = 0.05 // hysteresis: ignore changes under 5% of applied
	// gccHeadroom scales the GCC estimate before it becomes the video encoder
	// target. The estimate covers everything on the wire (video, audio, RTX
	// retransmissions, RTP/SRTP overhead); feeding it 1:1 to the encoder keeps
	// the link saturated and the queueing delay up.
	gccHeadroom = 0.85
)

// newABRRunner builds an abrRunner from config. It returns nil when ABR is
// disabled so the caller can skip wiring entirely.
func newABRRunner(log *slog.Logger, cfg config.Config, stats StatsSource, enc BitrateController) *abrRunner {
	if !cfg.ABR.Enabled {
		return nil
	}
	cooldown, interval := abrTimings(cfg, core.EncoderCaps{})
	acfg := abr.Config{
		Min:           cfg.ABR.MinBitrate,
		Max:           cfg.ABR.MaxBitrate,
		Start:         cfg.Encoder.Bitrate, // start from the configured fixed bitrate
		LossThreshold: cfg.ABR.LossThreshold,
		Cooldown:      cooldown,
	}
	return newABRRunnerWith(log, stats, enc, abr.New(acfg, nil), interval)
}

// newABRRunnerWith builds an abrRunner from an explicit controller and interval.
// It is the seam tests use to inject a controller with a fake clock / short
// cooldown so the full sample→decide→apply path can be asserted deterministically.
func newABRRunnerWith(log *slog.Logger, stats StatsSource, enc BitrateController, ctrl *abr.Controller, interval time.Duration) *abrRunner {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	return &abrRunner{
		log:      log.With("module", "abr"),
		stats:    stats,
		enc:      enc,
		ctrl:     ctrl,
		interval: interval,
	}
}

// run samples stats on a ticker, feeds the controller, and applies bitrate
// changes to the encoder. It blocks until ctx is cancelled. Invalid samples
// (no remote receiver report yet) are skipped so the controller is not fed
// bogus zero-loss data before the link has produced stats.
func (a *abrRunner) run(ctx context.Context) {
	a.log.Info("abr started",
		"start", a.ctrl.Target(),
		"min", a.ctrl.Config().Min,
		"max", a.ctrl.Config().Max,
		"interval", a.interval)
	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.tick()
		}
	}
}

// tick performs one sample→decide→apply step. Split out so it is unit-testable
// without a ticker.
func (a *abrRunner) tick() {
	var s rtc.StatsSample
	if a.signal != nil {
		sig := a.signal.Signal()
		if sig.TargetBps > 0 {
			a.tickGCC(sig)
			return
		}
		s = sig.Stats
	} else {
		s = a.stats.Stats()
	}
	if !s.Valid {
		return
	}
	d := a.ctrl.Observe(abr.Sample{LossFraction: s.LossFraction, RTT: s.RTT, Congested: s.Congested})
	if !d.Changed {
		return
	}
	a.log.Info("abr bitrate change",
		"reason", d.Reason.String(),
		"bitrate", d.Target,
		"loss", s.LossFraction,
		"rtt", s.RTT,
		"congested", s.Congested,
		"source", s.Source)
	a.enc.SetBitrate(d.Target)
}

// tickGCC follows a direct GCC target: clamp to [Min,Max], EWMA-smooth, then
// apply only when it differs from the applied bitrate by >= gccMinChange and the
// controller cooldown has elapsed. The AIMD controller is bypassed.
func (a *abrRunner) tickGCC(sig ABRSignal) {
	cfg := a.ctrl.Config()
	if a.now == nil {
		a.now = time.Now
	}
	t := int(float64(sig.TargetBps) * gccHeadroom)
	if t < cfg.Min {
		t = cfg.Min
	}
	if t > cfg.Max {
		t = cfg.Max
	}
	if a.gccApplied == 0 {
		a.gccApplied = a.ctrl.Target()
		a.gccSmoothed = float64(a.gccApplied)
	}
	a.gccSmoothed = gccEWMAAlpha*float64(t) + (1-gccEWMAAlpha)*a.gccSmoothed
	next := int(a.gccSmoothed)
	diff := next - a.gccApplied
	if diff < 0 {
		diff = -diff
	}
	if float64(diff) < gccMinChange*float64(a.gccApplied) {
		return
	}
	now := a.now()
	if !a.gccLast.IsZero() && now.Sub(a.gccLast) < cfg.Cooldown {
		return
	}
	a.log.Info("abr gcc bitrate change",
		"bitrate", next, "gcc_target", sig.TargetBps,
		"loss", sig.Stats.LossFraction, "rtt", sig.Stats.RTT)
	a.gccApplied = next
	a.gccLast = now
	a.enc.SetBitrate(next)
}
