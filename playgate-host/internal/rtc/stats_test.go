package rtc

import (
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

func TestSampleFromReportEmpty(t *testing.T) {
	if s := sampleFromReport(webrtc.StatsReport{}); s.Valid {
		t.Errorf("empty report should be invalid, got %+v", s)
	}
}

func TestSampleFromReportExtracts(t *testing.T) {
	report := webrtc.StatsReport{
		"outbound": webrtc.OutboundRTPStreamStats{},
		"remote-inbound": webrtc.RemoteInboundRTPStreamStats{
			FractionLost:  0.1,
			RoundTripTime: 0.080, // 80 ms in seconds
		},
	}
	s := sampleFromReport(report)
	if !s.Valid {
		t.Fatal("expected valid sample")
	}
	if s.LossFraction != 0.1 {
		t.Errorf("loss = %v, want 0.1", s.LossFraction)
	}
	if s.RTT != 80*time.Millisecond {
		t.Errorf("rtt = %v, want 80ms", s.RTT)
	}
}

func TestSampleFromReportPicksWorst(t *testing.T) {
	report := webrtc.StatsReport{
		"a": webrtc.RemoteInboundRTPStreamStats{FractionLost: 0.02, RoundTripTime: 0.01},
		"b": webrtc.RemoteInboundRTPStreamStats{FractionLost: 0.30, RoundTripTime: 0.05},
	}
	s := sampleFromReport(report)
	if s.LossFraction != 0.30 {
		t.Errorf("should pick worst loss 0.30, got %v", s.LossFraction)
	}
}

func TestSampleFromReportClampsLoss(t *testing.T) {
	report := webrtc.StatsReport{
		"x": webrtc.RemoteInboundRTPStreamStats{FractionLost: 1.5},
	}
	if s := sampleFromReport(report); s.LossFraction != 1.0 {
		t.Errorf("loss should clamp to 1.0, got %v", s.LossFraction)
	}
	report = webrtc.StatsReport{
		"x": webrtc.RemoteInboundRTPStreamStats{FractionLost: -0.5},
	}
	if s := sampleFromReport(report); s.LossFraction != 0.0 {
		t.Errorf("loss should clamp to 0.0, got %v", s.LossFraction)
	}
}
