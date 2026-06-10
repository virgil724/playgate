package abr

import (
	"testing"
	"time"
)

// fakeClock is a manually-advanced clock for deterministic ABR tests.
type fakeClock struct{ t time.Time }

func newClock() *fakeClock { return &fakeClock{t: time.Unix(0, 0)} }
func (c *fakeClock) now() time.Time   { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

// testConfig is a small, explicit config so the arithmetic in assertions is
// obvious: 1–8 Mbps, start 4 Mbps, decrease *0.5, +1 Mbps increase, 10s
// cooldown, 5s clean window, ignore <10% changes.
func testConfig() Config {
	return Config{
		Min:               1_000_000,
		Max:               8_000_000,
		Start:             4_000_000,
		LossThreshold:     0.05,
		DecreaseFactor:    0.5,
		IncreaseStep:      1_000_000,
		Cooldown:          10 * time.Second,
		CleanWindow:       5 * time.Second,
		MinChangeFraction: 0.1,
	}
}

func TestStartClampedAndDefaults(t *testing.T) {
	c := New(Config{}, newClock().now) // all defaults
	if c.Target() != DefaultConfig().Start {
		t.Errorf("default start = %d, want %d", c.Target(), DefaultConfig().Start)
	}

	// Start above Max is clamped down.
	clk := newClock()
	c = New(Config{Min: 1_000_000, Max: 3_000_000, Start: 9_000_000}, clk.now)
	if c.Target() != 3_000_000 {
		t.Errorf("start should clamp to max 3e6, got %d", c.Target())
	}
}

func TestDecreaseOnLoss(t *testing.T) {
	clk := newClock()
	c := New(testConfig(), clk.now)

	// First sample seeds timers; cooldown is measured from here. Advance past
	// cooldown so the decrease can commit.
	clk.add(11 * time.Second)
	d := c.Observe(Sample{LossFraction: 0.20}) // well above 5% threshold
	if !d.Changed || d.Reason != ReasonDecrease {
		t.Fatalf("expected decrease, got %+v", d)
	}
	if d.Target != 2_000_000 { // 4e6 * 0.5
		t.Errorf("target = %d, want 2000000", d.Target)
	}
}

func TestDecreaseClampedToMin(t *testing.T) {
	clk := newClock()
	cfg := testConfig()
	cfg.Start = 1_200_000 // *0.5 = 600k, below min 1e6 -> clamp to 1e6
	c := New(cfg, clk.now)

	clk.add(11 * time.Second)
	d := c.Observe(Sample{LossFraction: 0.5})
	if d.Target != 1_000_000 {
		t.Errorf("decrease should clamp to min 1e6, got %d", d.Target)
	}
}

func TestIncreaseAfterCleanWindow(t *testing.T) {
	clk := newClock()
	c := New(testConfig(), clk.now)

	// t=0: first clean sample seeds clean-window + cooldown timers.
	c.Observe(Sample{LossFraction: 0.0})

	// Before the clean window elapses (and cooldown), nothing happens.
	clk.add(3 * time.Second)
	if d := c.Observe(Sample{LossFraction: 0.0}); d.Changed {
		t.Fatalf("should not increase before clean window/cooldown: %+v", d)
	}

	// After cooldown (10s) AND clean window (5s), an increase commits.
	clk.add(8 * time.Second) // total 11s clean and since last change
	d := c.Observe(Sample{LossFraction: 0.0})
	if !d.Changed || d.Reason != ReasonIncrease {
		t.Fatalf("expected increase, got %+v", d)
	}
	if d.Target != 5_000_000 { // 4e6 + 1e6
		t.Errorf("target = %d, want 5000000", d.Target)
	}
}

func TestIncreaseClampedToMax(t *testing.T) {
	clk := newClock()
	cfg := testConfig()
	cfg.Start = 7_000_000 // +1e6 = 8e6 = max; delta 1e6 clears the 10% gate
	c := New(cfg, clk.now)

	c.Observe(Sample{LossFraction: 0.0}) // seed clean window at t=0
	clk.add(11 * time.Second)            // past cooldown + clean window
	d := c.Observe(Sample{LossFraction: 0.0})
	if d.Target != 8_000_000 {
		t.Errorf("increase should clamp to max 8e6, got %d", d.Target)
	}
}

func TestCooldownBlocksRapidChanges(t *testing.T) {
	clk := newClock()
	c := New(testConfig(), clk.now)

	clk.add(11 * time.Second)
	d1 := c.Observe(Sample{LossFraction: 0.5})
	if !d1.Changed {
		t.Fatal("first decrease should commit")
	}
	// Immediately after a change, even heavy loss is held off by the cooldown.
	clk.add(2 * time.Second)
	d2 := c.Observe(Sample{LossFraction: 0.9})
	if d2.Changed {
		t.Errorf("change within cooldown should be suppressed, got %+v", d2)
	}
	if d2.Target != d1.Target {
		t.Errorf("target should be unchanged during cooldown")
	}
	// Once cooldown passes, the next decrease commits.
	clk.add(9 * time.Second) // 11s since last change
	d3 := c.Observe(Sample{LossFraction: 0.9})
	if !d3.Changed {
		t.Errorf("decrease after cooldown should commit, got %+v", d3)
	}
}

func TestMinChangeFractionSuppressesTinyDeltas(t *testing.T) {
	clk := newClock()
	cfg := testConfig()
	cfg.IncreaseStep = 100_000 // 100k on a 4e6 base = 2.5% < 10% min change
	c := New(cfg, clk.now)

	c.Observe(Sample{LossFraction: 0.0}) // seed
	clk.add(20 * time.Second)
	d := c.Observe(Sample{LossFraction: 0.0})
	if d.Changed {
		t.Errorf("sub-threshold increase should be suppressed, got %+v", d)
	}
}

func TestSteadyCleanNoChurn(t *testing.T) {
	clk := newClock()
	cfg := testConfig()
	cfg.Start = cfg.Max // already at max; clean link can't increase further
	c := New(cfg, clk.now)

	c.Observe(Sample{LossFraction: 0.0})
	changes := 0
	for i := 0; i < 20; i++ {
		clk.add(11 * time.Second)
		if c.Observe(Sample{LossFraction: 0.0}).Changed {
			changes++
		}
	}
	if changes != 0 {
		t.Errorf("at max with clean link there should be no changes, got %d", changes)
	}
}

// TestLossThenRecoverySequence exercises a realistic trajectory: clean ramp-up,
// a loss burst forcing decreases, then recovery ramping back up.
func TestLossThenRecoverySequence(t *testing.T) {
	clk := newClock()
	c := New(testConfig(), clk.now)
	c.Observe(Sample{LossFraction: 0.0}) // seed at 4e6

	// Loss burst: two decreases (cooldown apart): 4e6 -> 2e6 -> 1e6 (clamped).
	clk.add(11 * time.Second)
	if d := c.Observe(Sample{LossFraction: 0.3}); d.Target != 2_000_000 {
		t.Fatalf("first decrease -> %d, want 2e6", d.Target)
	}
	clk.add(11 * time.Second)
	if d := c.Observe(Sample{LossFraction: 0.3}); d.Target != 1_000_000 {
		t.Fatalf("second decrease -> %d, want 1e6 (min)", d.Target)
	}

	// Recovery: the link must stay clean for the clean window before the first
	// increase. This sample (right after the loss burst) opens the clean window
	// but the window has not elapsed yet, so no increase commits.
	clk.add(11 * time.Second)
	if d := c.Observe(Sample{LossFraction: 0.0}); d.Changed {
		t.Fatalf("increase before clean window should be suppressed: %+v", d)
	}
	// Now the clean window has elapsed (and cooldown): +1e6 each tick.
	clk.add(11 * time.Second)
	if d := c.Observe(Sample{LossFraction: 0.0}); d.Target != 2_000_000 {
		t.Fatalf("first increase -> %d, want 2e6", d.Target)
	}
	clk.add(11 * time.Second)
	if d := c.Observe(Sample{LossFraction: 0.0}); d.Target != 3_000_000 {
		t.Fatalf("second increase -> %d, want 3e6", d.Target)
	}
}

func TestValidate(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Errorf("default config should validate: %v", err)
	}
	if err := (Config{Min: 0, Max: 1}).Validate(); err == nil {
		t.Error("expected error for non-positive min")
	}
	if err := (Config{Min: 5, Max: 1, LossThreshold: 0.1}).Validate(); err == nil {
		t.Error("expected error for max<min")
	}
	if err := (Config{Min: 1, Max: 2, LossThreshold: 1.5}).Validate(); err == nil {
		t.Error("expected error for loss threshold >= 1")
	}
}
