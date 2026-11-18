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
	// Congested is a delay/queue congestion signal independent of loss: the
	// path's RTT has grown well past its recent minimum, or the host's own send
	// queue overflowed. Set only for the WebCodecs (DataChannel) path, where
	// SCTP queues instead of dropping, so loss alone reacts far too late.
	Congested bool
	// Source names where the sample came from: "rtp" or "webcodecs".
	Source string
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
	report := p.pc.GetStats()
	if p.vs != nil && p.vs.Active() == VideoTransportWebCodecs {
		// No RTP flows on this path, so there are no receiver reports: use the
		// viewer's FRAME_REPORTs, our send queue and the SCTP RTT instead.
		return p.vs.linkStats(sctpSRTT(report))
	}
	out := sampleFromReport(report)
	out.Source = "rtp"
	return out
}

// sctpSRTT returns the SCTP association's smoothed RTT from a stats report, or
// 0 when unavailable.
func sctpSRTT(report webrtc.StatsReport) time.Duration {
	for _, s := range report {
		if st, ok := s.(webrtc.SCTPTransportStats); ok && st.SmoothedRoundTripTime > 0 {
			return time.Duration(st.SmoothedRoundTripTime * float64(time.Second))
		}
	}
	return 0
}

// Congestion thresholds for the WebCodecs path.
const (
	// rttWindow is how many recent SRTT samples define the "uncongested" RTT
	// baseline (at the ABR's 0.5-2s sampling, about 30-120s). A window rather
	// than an all-time minimum lets the baseline follow a network change.
	rttWindow = 60
	// queueDelayMin is the RTT growth over the baseline treated as queueing;
	// together with the 2x factor it ignores normal jitter on short paths.
	queueDelayMin = 60 * time.Millisecond
)

// linkStats folds the viewer's reports since the previous call and the SCTP
// RTT into an ABR sample for this viewer.
func (s *videoSender) linkStats(srtt time.Duration) StatsSample {
	ls := s.takeLinkSample()
	s.mu.Lock()
	base := s.recordRTT(srtt)
	bufMax := s.cfg.BufferMax
	s.mu.Unlock()
	return webcodecsSample(ls, srtt, base, bufMax)
}

// recordRTT adds srtt to the window and returns the window minimum (0 if
// none). Caller holds s.mu.
func (s *videoSender) recordRTT(srtt time.Duration) time.Duration {
	if srtt > 0 {
		s.rttWin = append(s.rttWin, srtt)
		if len(s.rttWin) > rttWindow {
			s.rttWin = s.rttWin[len(s.rttWin)-rttWindow:]
		}
	}
	var min time.Duration
	for _, r := range s.rttWin {
		if min == 0 || r < min {
			min = r
		}
	}
	return min
}

// webcodecsSample turns WebCodecs link counters into a StatsSample. Pure, for
// testing. Loss is fragment loss only: what the network actually dropped
// (before FEC). Frame loss is NOT a network signal: while the viewer waits for
// a keyframe it discards every arriving delta and reports it lost, so frame
// loss reads 100% on a perfectly clean link and would drive the bitrate to the
// floor. Congestion is a sustained RTT rise over the baseline, a send queue
// past half its limit, or any frame the host had to drop for a full queue.
func webcodecsSample(ls videoLinkSample, srtt, baseRTT time.Duration, bufferMax uint64) StatsSample {
	out := StatsSample{Source: "webcodecs", RTT: srtt}
	out.Valid = ls.Reports > 0 || ls.SenderDropped > 0
	if ls.FragsExpected > 0 && ls.FragsRecv < ls.FragsExpected {
		out.LossFraction = 1 - float64(ls.FragsRecv)/float64(ls.FragsExpected)
	}
	queueing := baseRTT > 0 && srtt > 2*baseRTT && srtt-baseRTT > queueDelayMin
	overfull := bufferMax > 0 && ls.Buffered > bufferMax/2
	out.Congested = queueing || overfull || ls.SenderDropped > 0
	if out.Congested {
		out.Valid = true
	}
	return out
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
