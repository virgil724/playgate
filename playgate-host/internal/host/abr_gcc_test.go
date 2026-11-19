package host

import (
	"testing"
	"time"

	"github.com/playgate/playgate-host/internal/abr"
	"github.com/playgate/playgate-host/internal/rtc"
)

type gccPeer struct {
	rtc.StatsSample
	bps int
	ok  bool
}

func (g gccPeer) Stats() rtc.StatsSample     { return g.StatsSample }
func (g gccPeer) TargetBitrate() (int, bool) { return g.bps, g.ok }

func TestRegistryGCCTargetSelection(t *testing.T) {
	ctrl := "a"
	r := newViewerStatsRegistry(func() string { return ctrl })
	r.gcc = true
	r.Register("a", gccPeer{smp(0, 0), 6_000_000, true})
	r.Register("b", gccPeer{smp(0, 0), 2_000_000, true})
	if got := r.Signal().TargetBps; got != 6_000_000 {
		t.Fatalf("controller should win, got %d", got)
	}
	ctrl = ""
	if got := r.Signal().TargetBps; got != 2_000_000 {
		t.Fatalf("min-of expected, got %d", got)
	}
	// Controller without estimate (e.g. webcodecs): min of those that have one.
	ctrl = "a"
	r.Register("a", gccPeer{smp(0, 0), 0, false})
	if got := r.Signal().TargetBps; got != 2_000_000 {
		t.Fatalf("fallback min expected, got %d", got)
	}
	// No estimates anywhere: TargetBps 0, stats still reported for AIMD.
	r.Register("b", constStats{smp(0.2, time.Millisecond)})
	sig := r.Signal()
	if sig.TargetBps != 0 || !sig.Stats.Valid {
		t.Fatalf("no estimates: want TargetBps 0 + stats, got %+v", sig)
	}
	// GCC off: never a target.
	r.gcc = false
	r.Register("a", gccPeer{smp(0, 0), 6_000_000, true})
	if r.Signal().TargetBps != 0 {
		t.Fatal("gcc off must not report TargetBps")
	}
}

type sigFn func() ABRSignal

func (f sigFn) Signal() ABRSignal { return f() }

func newGCCRunner(sig *ABRSignal, enc *recordingEncoder, clock *time.Time) *abrRunner {
	ctrl := abr.New(abr.Config{Min: 1_000_000, Max: 8_000_000, Start: 4_000_000, Cooldown: time.Second}, nil)
	r := newABRRunnerWith(discardLogger(), nil, enc, ctrl, time.Millisecond)
	r.signal = sigFn(func() ABRSignal { return *sig })
	r.now = func() time.Time { return *clock }
	return r
}

func TestABRTickGCCClampHysteresisCooldown(t *testing.T) {
	enc := &recordingEncoder{}
	clock := time.Unix(1000, 0)
	sig := &ABRSignal{Stats: smp(0, 0), TargetBps: 4_800_000} // *0.85 headroom = 4.08M
	r := newGCCRunner(sig, enc, &clock)

	// ~2% off the applied 4M after headroom: below the 5% hysteresis, no change.
	r.tick()
	if len(enc.calls()) != 0 {
		t.Fatalf("hysteresis violated: %v", enc.calls())
	}
	// Target far above Max is clamped to Max; EWMA converges gradually.
	sig.TargetBps = 50_000_000
	last := 0
	for i := 0; i < 40; i++ {
		clock = clock.Add(2 * time.Second)
		r.tick()
		if c := enc.calls(); len(c) > 0 {
			last = c[len(c)-1]
		}
	}
	if last < 7_000_000 || last > 8_000_000 {
		t.Fatalf("expected convergence near Max, last=%d calls=%v", last, enc.calls())
	}
	// Cooldown: after one applied change, further big moves within Cooldown
	// (1s) are deferred.
	n := len(enc.calls())
	sig.TargetBps = 1_000_000
	clock = clock.Add(2 * time.Second)
	r.tick()
	if c := enc.calls(); len(c) != n+1 || c[n] >= last {
		t.Fatalf("expected decrease: %v", c)
	}
	for i := 0; i < 10; i++ {
		clock = clock.Add(10 * time.Millisecond)
		r.tick()
	}
	if len(enc.calls()) != n+1 {
		t.Fatalf("cooldown violated: %v", enc.calls())
	}
	// Below Min is clamped to Min.
	for i := 0; i < 60; i++ {
		clock = clock.Add(2 * time.Second)
		sig.TargetBps = 10_000
		r.tick()
	}
	c := enc.calls()
	if c[len(c)-1] < 1_000_000 {
		t.Fatalf("below Min: %v", c)
	}
}

func TestABRTickGCCFallsBackToAIMDWithoutTarget(t *testing.T) {
	enc := &recordingEncoder{}
	clock := time.Unix(1000, 0)
	sig := &ABRSignal{Stats: smp(0.5, time.Millisecond)}
	r := newGCCRunner(sig, enc, &clock)
	r.ctrl = abr.New(abr.Config{Min: 1_000_000, Max: 8_000_000, Start: 4_000_000, LossThreshold: 0.05}, func() time.Time { return clock })
	clock = clock.Add(time.Minute)
	r.tick()
	c := enc.calls()
	if len(c) != 1 || c[0] >= 4_000_000 {
		t.Fatalf("AIMD decrease expected, got %v", c)
	}
}

// TestABRTickGCCHeadroom checks the encoder converges to gccHeadroom of the
// GCC estimate (which covers audio, RTX and packet overhead), not to the
// estimate itself.
func TestABRTickGCCHeadroom(t *testing.T) {
	enc := &recordingEncoder{}
	clock := time.Unix(1000, 0)
	sig := &ABRSignal{Stats: smp(0, 0), TargetBps: 6_000_000}
	r := newGCCRunner(sig, enc, &clock)
	for i := 0; i < 60; i++ {
		clock = clock.Add(2 * time.Second)
		r.tick()
	}
	c := enc.calls()
	if len(c) == 0 {
		t.Fatal("no bitrate change applied")
	}
	want := int(6_000_000 * gccHeadroom)
	if last := c[len(c)-1]; last < want*95/100 || last > want*105/100 {
		t.Fatalf("converged to %d, want ~%d (estimate %d * headroom)", last, want, sig.TargetBps)
	}
}
