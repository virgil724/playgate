package host

import (
	"context"
	"testing"
	"time"

	"github.com/playgate/playgate-host/internal/config"
	"github.com/playgate/playgate-host/internal/core"
	"github.com/playgate/playgate-host/internal/rtc"
)

type constStats struct{ s rtc.StatsSample }

func (c constStats) Stats() rtc.StatsSample { return c.s }

func smp(loss float64, rtt time.Duration) rtc.StatsSample {
	return rtc.StatsSample{LossFraction: loss, RTT: rtt, Valid: true}
}

func TestRegistryControllerWins(t *testing.T) {
	ctrl := "a"
	r := newViewerStatsRegistry(func() string { return ctrl })
	r.Register("a", constStats{smp(0.01, 10*time.Millisecond)})
	r.Register("b", constStats{smp(0.5, 300*time.Millisecond)})
	if got := r.Stats(); got.LossFraction != 0.01 {
		t.Fatalf("controller sample expected, got %+v", got)
	}
	// Controller invalid: falls back to worst-of.
	r.Register("a", constStats{})
	if got := r.Stats(); got.LossFraction != 0.5 {
		t.Fatalf("fallback worst expected, got %+v", got)
	}
}

func TestRegistryWorstOfWhenNoController(t *testing.T) {
	r := newViewerStatsRegistry(func() string { return "" })
	r.Register("a", constStats{smp(0.02, 50*time.Millisecond)})
	r.Register("b", constStats{smp(0.02, 200*time.Millisecond)})
	r.Register("c", constStats{smp(0.01, 900*time.Millisecond)})
	r.Register("bad", constStats{}) // invalid skipped
	got := r.Stats()
	if got.LossFraction != 0.02 || got.RTT != 200*time.Millisecond {
		t.Fatalf("worst = %+v", got)
	}
	// nil controller func also works.
	if got := newViewerStatsRegistry(nil).Stats(); got.Valid {
		t.Fatalf("empty must be invalid, got %+v", got)
	}
}

// TestWorseSampleRanksCongestionFirst checks a congested viewer (queueing on
// the WebCodecs path) outranks a lossier but uncongested one, so ABR backs off
// for the viewer whose link is actually overloaded.
func TestWorseSampleRanksCongestionFirst(t *testing.T) {
	congested := rtc.StatsSample{Valid: true, LossFraction: 0.01, Congested: true}
	lossy := rtc.StatsSample{Valid: true, LossFraction: 0.04}
	if !worseSample(congested, lossy) || worseSample(lossy, congested) {
		t.Error("congested sample should rank worse than a lossier uncongested one")
	}
	slow := rtc.StatsSample{Valid: true, LossFraction: 0.04, RTT: 90 * time.Millisecond}
	if !worseSample(slow, lossy) {
		t.Error("equal loss should fall back to higher RTT")
	}
}

func TestRegistryRegisterUnregister(t *testing.T) {
	r := newViewerStatsRegistry(nil)
	r.Register("a", constStats{smp(0.1, time.Millisecond)})
	if !r.Stats().Valid {
		t.Fatal("expected valid with one viewer")
	}
	r.Unregister("a")
	if r.Stats().Valid {
		t.Fatal("expected invalid after unregister")
	}
}

func TestABRTimingsByCaps(t *testing.T) {
	cfg := config.Default()
	cfg.ABR.CooldownSeconds = 10
	cfg.ABR.SampleIntervalMS = 0
	cd, iv := abrTimings(cfg, core.EncoderCaps{LiveBitrate: true})
	if cd != 500*time.Millisecond || iv != 500*time.Millisecond {
		t.Errorf("live: %v %v", cd, iv)
	}
	cd, iv = abrTimings(cfg, core.EncoderCaps{})
	if cd != 10*time.Second || iv != 2*time.Second {
		t.Errorf("cli: %v %v", cd, iv)
	}
	cfg.ABR.SampleIntervalMS = 1000
	_, iv = abrTimings(cfg, core.EncoderCaps{LiveBitrate: true})
	if iv != time.Second {
		t.Errorf("configured interval must win, got %v", iv)
	}
}

func TestABREmptySetHolds(t *testing.T) {
	enc := &recordingEncoder{}
	reg := newViewerStatsRegistry(nil)
	r := newABRRunner(discardLogger(), abrTestConfig(), reg, enc)
	r.signal = reg
	for i := 0; i < 20; i++ {
		r.tick()
	}
	enc.mu.Lock()
	defer enc.mu.Unlock()
	if len(enc.got) != 0 {
		t.Fatalf("no viewers must hold bitrate, got %v", enc.got)
	}
}

type capsEnc struct {
	*recordingEncoder
	caps core.EncoderCaps
}

func (c capsEnc) Caps() core.EncoderCaps { return c.caps }

func TestStartHostABRWiring(t *testing.T) {
	cfg := abrTestConfig()
	cfg.ABR.SampleIntervalMS = 5
	enc := &recordingEncoder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Disabled: nothing started.
	off := cfg
	off.ABR.Enabled = false
	if startHostABR(ctx, discardLogger(), off, enc, nil) != nil {
		t.Fatal("disabled must return nil")
	}

	reg := startHostABR(ctx, discardLogger(), cfg, capsEnc{enc, core.EncoderCaps{LiveBitrate: true}}, nil)
	if reg == nil {
		t.Fatal("enabled must return registry")
	}
	reg.Register("v1", constStats{smp(0.3, 20*time.Millisecond)})
	deadline := time.After(3 * time.Second)
	for {
		enc.mu.Lock()
		n := len(enc.got)
		first := 0
		if n > 0 {
			first = enc.got[0]
		}
		enc.mu.Unlock()
		if n > 0 {
			if first >= cfg.Encoder.Bitrate {
				t.Fatalf("expected decrease from %d, got %d", cfg.Encoder.Bitrate, first)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("ABR loop never applied a bitrate change")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
