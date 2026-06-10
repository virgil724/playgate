package host

import (
	"context"
	"log/slog"
	"time"

	"github.com/playgate/playgate-host/internal/abr"
	"github.com/playgate/playgate-host/internal/config"
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
}

// newABRRunner builds an abrRunner from config. It returns nil when ABR is
// disabled so the caller can skip wiring entirely.
func newABRRunner(log *slog.Logger, cfg config.Config, stats StatsSource, enc BitrateController) *abrRunner {
	if !cfg.ABR.Enabled {
		return nil
	}
	acfg := abr.Config{
		Min:           cfg.ABR.MinBitrate,
		Max:           cfg.ABR.MaxBitrate,
		Start:         cfg.Encoder.Bitrate, // start from the configured fixed bitrate
		LossThreshold: cfg.ABR.LossThreshold,
		Cooldown:      time.Duration(cfg.ABR.CooldownSeconds) * time.Second,
	}
	interval := time.Duration(cfg.ABR.SampleIntervalMS) * time.Millisecond
	if interval <= 0 {
		interval = 2 * time.Second
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
	s := a.stats.Stats()
	if !s.Valid {
		return
	}
	d := a.ctrl.Observe(abr.Sample{LossFraction: s.LossFraction, RTT: s.RTT})
	if !d.Changed {
		return
	}
	a.log.Info("abr bitrate change",
		"reason", d.Reason.String(),
		"bitrate", d.Target,
		"loss", s.LossFraction,
		"rtt", s.RTT)
	a.enc.SetBitrate(d.Target)
}
