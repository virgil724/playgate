// Package metrics provides lightweight, dependency-free latency instrumentation
// for the PlayGate Host pipeline. It deliberately avoids Prometheus or any
// external collector: each measured stage owns a Histogram backed by a fixed-size
// ring buffer, and a Reporter periodically logs p50/p95/max over the most recent
// samples.
//
// The pipeline stages instrumented are:
//
//	capture  — wall time a frame spent in the capture source before being read
//	encode   — wall time from a frame entering the encoder to its packet leaving
//	rtc      — wall time from a packet leaving the encoder to being written to
//	           the WebRTC track
//	e2e_rtt  — application-level round trip measured by the control-channel ping
//	           (see internal/rtc control ping handling)
//
// All methods are safe for concurrent use.
package metrics

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"
)

// ringSize is the number of recent samples each Histogram retains. At 60 fps a
// 600-sample window is ~10 s of history, enough for stable percentiles while
// staying tiny.
const ringSize = 600

// Histogram is a fixed-size ring buffer of duration samples with percentile
// queries. It overwrites the oldest sample once full.
type Histogram struct {
	mu     sync.Mutex
	buf    [ringSize]time.Duration
	n      int // number of samples written (caps at ringSize for length)
	next   int // next write index
	filled bool
}

// NewHistogram returns an empty Histogram.
func NewHistogram() *Histogram { return &Histogram{} }

// Observe records one duration sample.
func (h *Histogram) Observe(d time.Duration) {
	h.mu.Lock()
	h.buf[h.next] = d
	h.next = (h.next + 1) % ringSize
	if h.next == 0 {
		h.filled = true
	}
	if !h.filled {
		h.n = h.next
	} else {
		h.n = ringSize
	}
	h.mu.Unlock()
}

// Stats is a snapshot of a Histogram's percentile summary.
type Stats struct {
	Count int
	P50   time.Duration
	P95   time.Duration
	Max   time.Duration
}

// Snapshot computes percentile statistics over the retained samples. It returns
// a zero-Count Stats when no samples have been observed.
func (h *Histogram) Snapshot() Stats {
	h.mu.Lock()
	n := h.n
	if n == 0 {
		h.mu.Unlock()
		return Stats{}
	}
	samples := make([]time.Duration, n)
	copy(samples, h.buf[:n])
	h.mu.Unlock()

	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	return Stats{
		Count: n,
		P50:   percentile(samples, 0.50),
		P95:   percentile(samples, 0.95),
		Max:   samples[n-1],
	}
}

// percentile returns the p-th percentile (0..1) of a sorted slice using
// nearest-rank. samples must be non-empty and sorted ascending.
func percentile(samples []time.Duration, p float64) time.Duration {
	if len(samples) == 1 {
		return samples[0]
	}
	rank := int(p * float64(len(samples)-1))
	if rank < 0 {
		rank = 0
	}
	if rank >= len(samples) {
		rank = len(samples) - 1
	}
	return samples[rank]
}

// Collector groups the per-stage histograms for the host pipeline.
type Collector struct {
	Capture *Histogram
	Encode  *Histogram
	RTC     *Histogram
	E2ERTT  *Histogram
}

// NewCollector returns a Collector with all stages initialised.
func NewCollector() *Collector {
	return &Collector{
		Capture: NewHistogram(),
		Encode:  NewHistogram(),
		RTC:     NewHistogram(),
		E2ERTT:  NewHistogram(),
	}
}

// stageJSON is one stage's snapshot in the /metrics JSON, with durations in
// milliseconds for easy reading.
type stageJSON struct {
	N     int     `json:"n"`
	P50ms float64 `json:"p50_ms"`
	P95ms float64 `json:"p95_ms"`
}

func toStageJSON(s Stats) stageJSON {
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	return stageJSON{N: s.Count, P50ms: ms(s.P50), P95ms: ms(s.P95)}
}

// ServeMetrics is an http.HandlerFunc that returns the current per-stage latency
// snapshot as JSON. Intended to be served on a localhost-only address so the
// pipeline latency can be inspected live without parsing logs.
func (c *Collector) ServeMetrics(w http.ResponseWriter, _ *http.Request) {
	body := map[string]stageJSON{
		"capture": toStageJSON(c.Capture.Snapshot()),
		"encode":  toStageJSON(c.Encode.Snapshot()),
		"rtc":     toStageJSON(c.RTC.Snapshot()),
		"e2e_rtt": toStageJSON(c.E2ERTT.Snapshot()),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// Report logs a one-line summary of every stage at level Info. It is safe to
// call from any goroutine.
func (c *Collector) Report(log *slog.Logger) {
	cap, enc, rtc, e2e := c.Capture.Snapshot(), c.Encode.Snapshot(), c.RTC.Snapshot(), c.E2ERTT.Snapshot()
	log.Info("latency",
		"capture_n", cap.Count, "capture_p50", cap.P50, "capture_p95", cap.P95,
		"encode_n", enc.Count, "encode_p50", enc.P50, "encode_p95", enc.P95,
		"rtc_n", rtc.Count, "rtc_p50", rtc.P50, "rtc_p95", rtc.P95,
		"e2e_rtt_n", e2e.Count, "e2e_rtt_p50", e2e.P50, "e2e_rtt_p95", e2e.P95,
	)
}

// RunReporter periodically logs the collector's stats until ctx is cancelled. It
// implements the body of a core.Module-style Run (the host wraps it). interval
// defaults to 5s when non-positive.
func (c *Collector) RunReporter(ctx context.Context, log *slog.Logger, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.Report(log)
		}
	}
}
