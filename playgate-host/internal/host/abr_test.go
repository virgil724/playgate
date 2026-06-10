package host

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/playgate/playgate-host/internal/abr"
	"github.com/playgate/playgate-host/internal/config"
	"github.com/playgate/playgate-host/internal/rtc"
)

// fakeStats returns a scripted sequence of StatsSamples, one per call, repeating
// the last entry once exhausted.
type fakeStats struct {
	mu      sync.Mutex
	seq     []rtc.StatsSample
	callIdx int
}

func (f *fakeStats) Stats() rtc.StatsSample {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.callIdx
	if i >= len(f.seq) {
		i = len(f.seq) - 1
	}
	f.callIdx++
	return f.seq[i]
}

// recordingEncoder records every SetBitrate call.
type recordingEncoder struct {
	mu  sync.Mutex
	got []int
}

func (r *recordingEncoder) SetBitrate(bps int) {
	r.mu.Lock()
	r.got = append(r.got, bps)
	r.mu.Unlock()
}
func (r *recordingEncoder) calls() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.got...)
}

func abrTestConfig() config.Config {
	cfg := config.Default()
	cfg.Encoder.Bitrate = 4_000_000
	cfg.ABR.Enabled = true
	cfg.ABR.MinBitrate = 1_000_000
	cfg.ABR.MaxBitrate = 8_000_000
	cfg.ABR.LossThreshold = 0.05
	cfg.ABR.CooldownSeconds = 10
	cfg.ABR.SampleIntervalMS = 1
	return cfg
}

// TestABRRunnerDisabledReturnsNil verifies ABR wiring is skipped when disabled.
func TestABRRunnerDisabledReturnsNil(t *testing.T) {
	cfg := config.Default() // ABR disabled
	if r := newABRRunner(discardLogger(), cfg, &fakeStats{seq: []rtc.StatsSample{{}}}, &recordingEncoder{}); r != nil {
		t.Error("ABR disabled should yield a nil runner")
	}
}

// TestABRRunnerEndToEndDecrease is the T14 integration test: a scripted fake
// stats sequence flows through a real abr.Controller (with an injected fake
// clock and short cooldown) into a recording encoder, and we assert the encoder
// receives the expected SetBitrate sequence. This is the fake-stats → abr →
// SetBitrate path the acceptance criteria call for, made deterministic by the
// injectable clock.
func TestABRRunnerEndToEndDecrease(t *testing.T) {
	clk := &testClock{t: time.Unix(0, 0)}
	ctrl := abr.New(abr.Config{
		Min:            1_000_000,
		Max:            8_000_000,
		Start:          4_000_000,
		LossThreshold:  0.05,
		DecreaseFactor: 0.5,
		IncreaseStep:   1_000_000,
		Cooldown:       10 * time.Second,
		CleanWindow:    5 * time.Second,
	}, clk.now)

	// Two heavy-loss samples then clean recovery.
	stats := &fakeStats{seq: []rtc.StatsSample{
		{LossFraction: 0.30, RTT: 50 * time.Millisecond, Valid: true}, // -> 2e6
		{LossFraction: 0.30, RTT: 50 * time.Millisecond, Valid: true}, // -> 1e6
		{LossFraction: 0.00, RTT: 20 * time.Millisecond, Valid: true}, // opens clean window
		{LossFraction: 0.00, RTT: 20 * time.Millisecond, Valid: true}, // -> 2e6 (increase)
	}}
	enc := &recordingEncoder{}
	runner := newABRRunnerWith(discardLogger(), stats, enc, ctrl, time.Millisecond)

	// Drive tick() manually, advancing the fake clock past the cooldown each time.
	clk.add(11 * time.Second)
	runner.tick() // loss -> decrease to 2e6
	clk.add(11 * time.Second)
	runner.tick() // loss -> decrease to 1e6
	clk.add(11 * time.Second)
	runner.tick() // clean: opens window, no change yet
	clk.add(11 * time.Second)
	runner.tick() // clean + window elapsed -> increase to 2e6

	got := enc.calls()
	want := []int{2_000_000, 1_000_000, 2_000_000}
	if len(got) != len(want) {
		t.Fatalf("SetBitrate calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SetBitrate[%d] = %d, want %d (all: %v)", i, got[i], want[i], got)
		}
	}
}

// testClock is a manually-advanced clock for deterministic ABR wiring tests.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// TestABRRunnerInvalidSampleSkipped verifies that an invalid stats sample (no
// remote report yet) never reaches the controller / encoder.
func TestABRRunnerInvalidSampleSkipped(t *testing.T) {
	stats := &fakeStats{seq: []rtc.StatsSample{{Valid: false}}}
	enc := &recordingEncoder{}
	runner := newABRRunner(discardLogger(), abrTestConfig(), stats, enc)
	runner.tick()
	if len(enc.calls()) != 0 {
		t.Errorf("invalid sample should be skipped, got %v", enc.calls())
	}
}

// TestABRRunnerLoopStopsOnCancel exercises the loop lifecycle: it starts,
// samples on the ticker, and stops cleanly on context cancel without leaking.
func TestABRRunnerLoopStopsOnCancel(t *testing.T) {
	stats := &fakeStats{seq: []rtc.StatsSample{
		{LossFraction: 0.0, RTT: 10 * time.Millisecond, Valid: true},
	}}
	enc := &recordingEncoder{}
	runner := newABRRunner(discardLogger(), abrTestConfig(), stats, enc)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runner.run(ctx); close(done) }()

	time.Sleep(20 * time.Millisecond) // let several ticks fire
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("abr runner did not stop on cancel")
	}
	// Some samples were taken (the fake counts calls).
	stats.mu.Lock()
	calls := stats.callIdx
	stats.mu.Unlock()
	if calls == 0 {
		t.Error("expected the runner to sample stats at least once")
	}
}
