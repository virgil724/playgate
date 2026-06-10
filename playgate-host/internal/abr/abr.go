// Package abr implements an adaptive bitrate (ABR) control loop for the PlayGate
// Host video encoder. It consumes periodic WebRTC transport statistics (packet
// loss fraction and round-trip time) and produces a target H.264 bitrate using a
// simple, robust AIMD (additive-increase / multiplicative-decrease) algorithm —
// the same congestion-control family TCP uses.
//
// Design goals:
//   - Pure and deterministic: the controller has no I/O and an injectable clock,
//     so every branch (increase / decrease / clamp / cooldown) is unit-testable.
//   - Stable, not twitchy: bitrate changes are debounced by a cooldown so the
//     encoder is not restarted constantly, and increases are gated by a sustained
//     clean window so a single good sample doesn't cause oscillation.
//   - Bounded: the output is always clamped to [Min, Max].
//
// The controller does not own the encoder or the stats source — the host wires
// a stats sampler to Observe and applies Target via the encoder-restart path.
package abr

import (
	"fmt"
	"time"
)

// Sample is one WebRTC transport statistics observation fed to the controller.
type Sample struct {
	// LossFraction is the fraction of packets lost since the previous sample,
	// in [0,1] (e.g. 0.05 = 5% loss).
	LossFraction float64
	// RTT is the most recent round-trip time. Currently advisory: high RTT alone
	// does not trigger a decrease (loss is the primary signal), but it is retained
	// for future policy and exposed on Decision for logging.
	RTT time.Duration
}

// Config parameters for the AIMD controller. Zero values are replaced by
// DefaultConfig values in New.
type Config struct {
	// Min and Max bound the output bitrate (bits per second).
	Min int
	Max int
	// Start is the initial target bitrate. Clamped into [Min,Max].
	Start int

	// LossThreshold is the loss fraction above which the controller decreases
	// the bitrate (multiplicative decrease). e.g. 0.05 = 5%.
	LossThreshold float64

	// DecreaseFactor multiplies the current bitrate on a decrease (0<f<1).
	// e.g. 0.8 drops to 80%.
	DecreaseFactor float64

	// IncreaseStep is the additive increase (bits per second) applied per
	// increase tick once the link has been clean long enough.
	IncreaseStep int

	// Cooldown is the minimum wall-clock time between two bitrate changes. It
	// debounces the encoder restart so the stream is not perturbed too often.
	Cooldown time.Duration

	// CleanWindow is how long loss must stay at/below LossThreshold before an
	// additive increase is allowed, to avoid ramping up into a still-congested
	// link.
	CleanWindow time.Duration

	// MinChangeFraction suppresses changes smaller than this fraction of the
	// current bitrate, so trivial deltas don't trigger an encoder restart.
	// e.g. 0.1 = ignore <10% changes.
	MinChangeFraction float64
}

// DefaultConfig returns sensible defaults: 1–8 Mbps, start 4 Mbps, decrease to
// 80% above 5% loss, +500 kbps additive increase, 10s cooldown, 5s clean window.
func DefaultConfig() Config {
	return Config{
		Min:               1_000_000,
		Max:               8_000_000,
		Start:             4_000_000,
		LossThreshold:     0.05,
		DecreaseFactor:    0.8,
		IncreaseStep:      500_000,
		Cooldown:          10 * time.Second,
		CleanWindow:       5 * time.Second,
		MinChangeFraction: 0.1,
	}
}

// normalise fills zero-valued fields from DefaultConfig and clamps Start.
func (c Config) normalise() Config {
	d := DefaultConfig()
	if c.Min <= 0 {
		c.Min = d.Min
	}
	if c.Max <= 0 {
		c.Max = d.Max
	}
	if c.Max < c.Min {
		c.Max = c.Min
	}
	if c.Start <= 0 {
		c.Start = d.Start
	}
	if c.LossThreshold <= 0 {
		c.LossThreshold = d.LossThreshold
	}
	if c.DecreaseFactor <= 0 || c.DecreaseFactor >= 1 {
		c.DecreaseFactor = d.DecreaseFactor
	}
	if c.IncreaseStep <= 0 {
		c.IncreaseStep = d.IncreaseStep
	}
	if c.Cooldown <= 0 {
		c.Cooldown = d.Cooldown
	}
	if c.CleanWindow <= 0 {
		c.CleanWindow = d.CleanWindow
	}
	if c.MinChangeFraction < 0 {
		c.MinChangeFraction = d.MinChangeFraction
	}
	c.Start = clamp(c.Start, c.Min, c.Max)
	return c
}

// Validate checks the configuration for obviously-wrong values.
func (c Config) Validate() error {
	if c.Min <= 0 || c.Max <= 0 {
		return fmt.Errorf("abr: min/max bitrate must be positive (min=%d max=%d)", c.Min, c.Max)
	}
	if c.Max < c.Min {
		return fmt.Errorf("abr: max bitrate %d < min %d", c.Max, c.Min)
	}
	if c.LossThreshold <= 0 || c.LossThreshold >= 1 {
		return fmt.Errorf("abr: loss_threshold must be in (0,1), got %v", c.LossThreshold)
	}
	return nil
}

// Reason classifies why a Decision changed (or did not change) the bitrate.
type Reason int

const (
	// ReasonNone: no change (within cooldown, sub-threshold delta, or steady).
	ReasonNone Reason = iota
	// ReasonDecrease: loss exceeded the threshold; multiplicative decrease.
	ReasonDecrease
	// ReasonIncrease: link stayed clean for the window; additive increase.
	ReasonIncrease
)

// String renders a Reason for logging.
func (r Reason) String() string {
	switch r {
	case ReasonDecrease:
		return "decrease"
	case ReasonIncrease:
		return "increase"
	default:
		return "none"
	}
}

// Decision is the result of feeding one Sample to the controller.
type Decision struct {
	// Target is the (possibly unchanged) target bitrate in bits per second.
	Target int
	// Changed is true when Target differs from the previous target AND the change
	// was actually committed (passed cooldown + min-change gating).
	Changed bool
	// Reason explains the decision.
	Reason Reason
}

// Controller is the AIMD ABR state machine. It is NOT safe for concurrent use;
// the host calls Observe from a single sampling goroutine.
type Controller struct {
	cfg Config
	now func() time.Time

	target      int
	lastChange  time.Time // wall time of the last committed bitrate change
	cleanSince  time.Time // wall time loss last dropped to/under threshold
	haveCleanTS bool
}

// New constructs a Controller. If now is nil, time.Now is used. Config is
// normalised so zero values get defaults.
func New(cfg Config, now func() time.Time) *Controller {
	if now == nil {
		now = time.Now
	}
	cfg = cfg.normalise()
	return &Controller{
		cfg:        cfg,
		now:        now,
		target:     cfg.Start,
		lastChange: now(), // seed cooldown from construction time
	}
}

// Target returns the current target bitrate.
func (c *Controller) Target() int { return c.target }

// Config returns the normalised configuration (mainly for tests / logging).
func (c *Controller) Config() Config { return c.cfg }

// Observe feeds one statistics sample and returns the resulting Decision. The
// algorithm:
//
//	if loss > threshold:
//	    target = max(Min, target * DecreaseFactor)   // multiplicative decrease
//	    reset the clean-window timer
//	else (loss is acceptable):
//	    if the link has been clean for >= CleanWindow:
//	        target = min(Max, target + IncreaseStep)  // additive increase
//
// Every change is gated by Cooldown (no two changes closer than Cooldown apart)
// and by MinChangeFraction (ignore deltas smaller than that fraction of the
// current target). Decreases are reactive and ALSO subject to cooldown so a loss
// burst can't collapse the bitrate in one tick storm; the clean-window timer is
// always updated regardless of whether a change is committed.
func (c *Controller) Observe(s Sample) Decision {
	now := c.now()

	loss := s.LossFraction

	// Maintain the clean-window timer independently of cooldown so that once
	// cooldown expires an already-clean link can increase immediately.
	if loss > c.cfg.LossThreshold {
		c.haveCleanTS = false
	} else if !c.haveCleanTS {
		c.cleanSince = now
		c.haveCleanTS = true
	}

	// Cooldown gate: never change more often than Cooldown.
	if now.Sub(c.lastChange) < c.cfg.Cooldown {
		return Decision{Target: c.target, Changed: false, Reason: ReasonNone}
	}

	var candidate int
	var reason Reason
	switch {
	case loss > c.cfg.LossThreshold:
		candidate = int(float64(c.target) * c.cfg.DecreaseFactor)
		reason = ReasonDecrease
	case c.haveCleanTS && now.Sub(c.cleanSince) >= c.cfg.CleanWindow:
		candidate = c.target + c.cfg.IncreaseStep
		reason = ReasonIncrease
	default:
		return Decision{Target: c.target, Changed: false, Reason: ReasonNone}
	}

	candidate = clamp(candidate, c.cfg.Min, c.cfg.Max)

	// Min-change gate: ignore deltas too small to be worth an encoder restart.
	delta := candidate - c.target
	if delta < 0 {
		delta = -delta
	}
	if candidate == c.target || float64(delta) < c.cfg.MinChangeFraction*float64(c.target) {
		return Decision{Target: c.target, Changed: false, Reason: ReasonNone}
	}

	c.target = candidate
	c.lastChange = now
	// After an increase, restart the clean window so the next increase needs a
	// fresh clean stretch (gentle ramp). After a decrease, the clean window was
	// already reset above when loss was high.
	if reason == ReasonIncrease {
		c.cleanSince = now
	}
	return Decision{Target: candidate, Changed: true, Reason: reason}
}

// clamp bounds v to [lo,hi].
func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
