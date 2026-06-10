package metrics

import (
	"testing"
	"time"
)

func TestHistogramEmpty(t *testing.T) {
	h := NewHistogram()
	s := h.Snapshot()
	if s.Count != 0 {
		t.Fatalf("empty histogram count = %d, want 0", s.Count)
	}
}

func TestHistogramPercentiles(t *testing.T) {
	h := NewHistogram()
	// Observe 1..100 ms.
	for i := 1; i <= 100; i++ {
		h.Observe(time.Duration(i) * time.Millisecond)
	}
	s := h.Snapshot()
	if s.Count != 100 {
		t.Fatalf("count = %d, want 100", s.Count)
	}
	if s.Max != 100*time.Millisecond {
		t.Errorf("max = %v, want 100ms", s.Max)
	}
	// nearest-rank p50 over 100 sorted samples → index 49 → 50ms.
	if s.P50 != 50*time.Millisecond {
		t.Errorf("p50 = %v, want 50ms", s.P50)
	}
	// p95 → index 94 → 95ms.
	if s.P95 != 95*time.Millisecond {
		t.Errorf("p95 = %v, want 95ms", s.P95)
	}
}

func TestHistogramRingWraps(t *testing.T) {
	h := NewHistogram()
	// Observe more than ringSize; only the most recent ringSize are retained.
	total := ringSize + 250
	for i := 0; i < total; i++ {
		h.Observe(time.Duration(i) * time.Millisecond)
	}
	s := h.Snapshot()
	if s.Count != ringSize {
		t.Fatalf("count = %d, want %d", s.Count, ringSize)
	}
	// The oldest retained sample is total-ringSize; max should be total-1.
	if s.Max != time.Duration(total-1)*time.Millisecond {
		t.Errorf("max = %v, want %v", s.Max, time.Duration(total-1)*time.Millisecond)
	}
}

func TestCollectorReportDoesNotPanic(t *testing.T) {
	c := NewCollector()
	c.Capture.Observe(5 * time.Millisecond)
	c.Encode.Observe(8 * time.Millisecond)
	// Report with a discard-ish logger: just ensure it runs without panicking.
	c.Report(testLogger())
}
