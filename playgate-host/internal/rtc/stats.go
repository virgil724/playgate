package rtc

import (
	"time"

	"github.com/pion/webrtc/v4"
)

// StatsSample is a transport-quality snapshot derived from the PeerConnection's
// stats: the most recent packet-loss fraction and round-trip time reported by
// the remote (viewer) for our outbound video stream. It is the input the ABR
// controller needs, expressed without importing the abr package so rtc stays a
// leaf of the dependency graph.
type StatsSample struct {
	// LossFraction is the remote-reported fraction of lost packets in [0,1].
	LossFraction float64
	// RTT is the remote-inbound round-trip time estimate.
	RTT time.Duration
	// Valid is false when no remote-inbound video stats were present yet (e.g.
	// before the first RTCP receiver report arrives).
	Valid bool
}

// Stats queries the underlying PeerConnection and extracts an ABR StatsSample.
//
// Stats source tradeoff: we use PeerConnection.GetStats() (the W3C stats model
// Pion implements) rather than installing a custom RTCP interceptor. GetStats is
// far cheaper to wire and to test — Pion already aggregates inbound RTCP Receiver
// Reports into RemoteInboundRTPStreamStats (fractionLost, roundTripTime) for our
// outbound video track — and the pure extraction in sampleFromReport is unit-
// testable with a hand-built StatsReport, no real network required. The cost is
// coarser granularity (we poll on a timer instead of reacting to each RR), which
// is exactly what the ABR cooldown wants anyway.
func (p *Peer) Stats() StatsSample {
	return sampleFromReport(p.pc.GetStats())
}

// sampleFromReport extracts the loss fraction and RTT from the remote-inbound
// video stats in a StatsReport. It is pure (no I/O) so it can be unit-tested.
// When several remote-inbound entries exist (unlikely for our single track) the
// one with the highest loss is used, biasing the controller toward the worst
// observed path.
func sampleFromReport(report webrtc.StatsReport) StatsSample {
	var out StatsSample
	for _, s := range report {
		ri, ok := s.(webrtc.RemoteInboundRTPStreamStats)
		if !ok {
			continue
		}
		loss := ri.FractionLost
		if loss < 0 {
			loss = 0
		}
		if loss > 1 {
			loss = 1
		}
		if !out.Valid || loss > out.LossFraction {
			out.LossFraction = loss
			// RoundTripTime is in seconds (float64) per the W3C stats model.
			out.RTT = time.Duration(ri.RoundTripTime * float64(time.Second))
			out.Valid = true
		}
	}
	return out
}
