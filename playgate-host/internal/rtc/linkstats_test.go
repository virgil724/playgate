package rtc

import (
	"encoding/binary"
	"testing"
	"time"
)

func TestWebCodecsSample(t *testing.T) {
	const bufMax = 256 * 1024
	ms := time.Millisecond
	cases := []struct {
		name          string
		ls            videoLinkSample
		srtt, base    time.Duration
		wantValid     bool
		wantLoss      float64
		wantCongested bool
	}{
		{name: "no data", wantValid: false},
		{name: "clean", ls: videoLinkSample{Reports: 2, FramesComplete: 30, FragsRecv: 300, FragsExpected: 300},
			srtt: 12 * ms, base: 10 * ms, wantValid: true},
		{name: "fragment loss", ls: videoLinkSample{Reports: 2, FramesComplete: 30, FragsRecv: 270, FragsExpected: 300},
			srtt: 12 * ms, base: 10 * ms, wantValid: true, wantLoss: 0.1},
		{name: "frame loss worse than fragment loss", ls: videoLinkSample{Reports: 2, FramesComplete: 24, FramesLost: 6, FragsRecv: 297, FragsExpected: 300},
			srtt: 12 * ms, base: 10 * ms, wantValid: true, wantLoss: 0.2},
		{name: "jitter on a short path is not queueing", ls: videoLinkSample{Reports: 2, FramesComplete: 30},
			srtt: 45 * ms, base: 10 * ms, wantValid: true},
		{name: "queueing delay", ls: videoLinkSample{Reports: 2, FramesComplete: 30},
			srtt: 400 * ms, base: 30 * ms, wantValid: true, wantCongested: true},
		{name: "send queue over half full", ls: videoLinkSample{Reports: 1, FramesComplete: 30, Buffered: bufMax/2 + 1},
			srtt: 30 * ms, base: 30 * ms, wantValid: true, wantCongested: true},
		{name: "host dropped frames, no reports got through", ls: videoLinkSample{SenderDropped: 3},
			wantValid: true, wantCongested: true},
	}
	for _, c := range cases {
		got := webcodecsSample(c.ls, c.srtt, c.base, bufMax)
		if got.Valid != c.wantValid || got.Congested != c.wantCongested || got.Source != "webcodecs" {
			t.Errorf("%s: got %+v, want valid=%v congested=%v", c.name, got, c.wantValid, c.wantCongested)
		}
		if diff := got.LossFraction - c.wantLoss; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("%s: loss = %v, want %v", c.name, got.LossFraction, c.wantLoss)
		}
	}
}

// frameReport builds a FRAME_REPORT message (see docs/webcodecs-transport.md 4.2).
func frameReport(complete, lost, recv, expected, jitterMSx10 uint16) []byte {
	b := make([]byte, FrameReportSize)
	b[0], b[1] = 1, FBFrameReport
	binary.BigEndian.PutUint16(b[8:], complete)
	binary.BigEndian.PutUint16(b[10:], lost)
	binary.BigEndian.PutUint16(b[12:], recv)
	binary.BigEndian.PutUint16(b[14:], expected)
	binary.BigEndian.PutUint16(b[20:], jitterMSx10)
	return b
}

// TestLinkSampleAccumulatesReports checks every FRAME_REPORT (each covering
// only ~250ms) is counted even when the ABR samples less often, and that
// taking a sample resets the counters.
func TestLinkSampleAccumulatesReports(t *testing.T) {
	s := newVideoSender(videoSenderConfig{Mode: VideoTransportWebCodecs})
	s.handleFeedback(frameReport(15, 0, 150, 150, 20))
	s.handleFeedback(frameReport(14, 1, 140, 150, 45))
	ls := s.takeLinkSample()
	want := videoLinkSample{FramesComplete: 29, FramesLost: 1, FragsRecv: 290, FragsExpected: 300,
		Jitter: 4500 * time.Microsecond, Reports: 2}
	if ls != want {
		t.Fatalf("got %+v, want %+v", ls, want)
	}
	if again := s.takeLinkSample(); again.Reports != 0 || again.FramesComplete != 0 {
		t.Errorf("counters not reset: %+v", again)
	}
}

// TestRTTBaselineIsWindowed checks the uncongested-RTT baseline follows a
// path change instead of sticking to an old all-time minimum.
func TestRTTBaselineIsWindowed(t *testing.T) {
	s := newVideoSender(videoSenderConfig{})
	s.recordRTT(5 * time.Millisecond)
	for i := 0; i < rttWindow; i++ {
		s.recordRTT(40 * time.Millisecond)
	}
	if base := s.recordRTT(0); base != 40*time.Millisecond {
		t.Errorf("baseline = %v, want 40ms once the 5ms sample left the window", base)
	}
}
