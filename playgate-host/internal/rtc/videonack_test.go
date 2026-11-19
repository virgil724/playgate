package rtc

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/pion/webrtc/v4"
)

func fragNackMsg(epoch uint16, frameID uint32, idx ...uint16) []byte {
	b := make([]byte, FragNackMinSize+2*len(idx))
	b[0], b[1] = fbVersion, FBFragNack
	binary.BigEndian.PutUint16(b[4:], epoch)
	binary.BigEndian.PutUint16(b[6:], uint16(len(idx)))
	binary.BigEndian.PutUint32(b[8:], frameID)
	for i, j := range idx {
		binary.BigEndian.PutUint16(b[FragNackMinSize+2*i:], j)
	}
	return b
}

func TestParseFragNack(t *testing.T) {
	got, err := ParseFeedback(fragNackMsg(3, 77, 0, 2))
	if err != nil {
		t.Fatal(err)
	}
	n, ok := got.(FragNack)
	if !ok || n.Epoch != 3 || n.FrameID != 77 || len(n.Indices) != 2 || n.Indices[1] != 2 {
		t.Fatalf("got %#v", got)
	}
	bad := fragNackMsg(3, 77, 0, 2)
	if _, err := ParseFeedback(bad[:len(bad)-1]); err == nil {
		t.Error("truncated index list accepted")
	}
}

// TestRetransmitOnFragNack checks the host resends exactly the requested
// fragments of a kept frame, the whole frame for an empty list, nothing for an
// unknown frame or stale epoch, and at most retransmitMaxTimes per fragment.
func TestRetransmitOnFragNack(t *testing.T) {
	dc := &fakeDC{state: webrtc.DataChannelStateOpen}
	s := newVideoSender(videoSenderConfig{Mode: VideoTransportWebCodecs, BufferMax: 1 << 20, IDRPaceMS: -1})
	s.setChannel(dc)
	s.handleFeedback(helloMsg(CapWebCodecsH264 | CapFragNack))
	_ = s.writeFrame(pkt(testAU(3000, true), 0, true)) // k=3
	sent := dc.take()
	if len(sent) != 3 {
		t.Fatalf("sent %d fragments, want 3", len(sent))
	}
	h, _, err := ParseVideoHeader(sent[0])
	if err != nil {
		t.Fatal(err)
	}

	s.handleFeedback(fragNackMsg(h.Epoch, h.FrameID, 1))
	if got := dc.take(); len(got) != 1 || !bytes.Equal(got[0], sent[1]) {
		t.Fatalf("index 1: got %d messages", len(got))
	}
	s.handleFeedback(fragNackMsg(h.Epoch, h.FrameID))
	if got := dc.take(); len(got) != 3 {
		t.Fatalf("whole frame: got %d messages, want 3", len(got))
	}
	// Fragment 1 has now been resent twice: no third time.
	s.handleFeedback(fragNackMsg(h.Epoch, h.FrameID, 1))
	if got := dc.take(); len(got) != 0 {
		t.Fatalf("over the resend cap: got %d messages", len(got))
	}
	s.handleFeedback(fragNackMsg(h.Epoch, h.FrameID+5, 0))
	s.handleFeedback(fragNackMsg(h.Epoch+1, h.FrameID, 0))
	if got := dc.take(); len(got) != 0 {
		t.Fatalf("unknown frame / epoch: got %d messages", len(got))
	}
}

// TestRetransmitGuards checks duplicate indices are resent once, a sender
// that switched to the RTP track resends nothing, and a link that went down
// drops the retransmit cache.
func TestRetransmitGuards(t *testing.T) {
	dc := &fakeDC{state: webrtc.DataChannelStateOpen}
	s := newVideoSender(videoSenderConfig{Mode: VideoTransportAuto, BufferMax: 1 << 20, IDRPaceMS: -1})
	s.setChannel(dc)
	s.handleFeedback(helloMsg(CapWebCodecsH264 | CapFragNack))
	s.switchTo(VideoTransportWebCodecs)
	_ = s.writeFrame(pkt(testAU(3000, true), 0, true))
	sent := dc.take()
	h, _, err := ParseVideoHeader(sent[0])
	if err != nil {
		t.Fatal(err)
	}

	s.handleFeedback(fragNackMsg(h.Epoch, h.FrameID, 2, 2))
	if got := dc.take(); len(got) != 1 {
		t.Fatalf("duplicate index: got %d messages, want 1", len(got))
	}

	s.setLinkUp(false)
	s.setLinkUp(true)
	s.handleFeedback(fragNackMsg(h.Epoch, h.FrameID, 0))
	if got := dc.take(); len(got) != 0 {
		t.Fatalf("after link down: got %d messages, want 0", len(got))
	}

	_ = s.writeFrame(pkt(testAU(3000, true), 0, true))
	h2, _, _ := ParseVideoHeader(dc.take()[0])
	var gateReset bool
	s.cfg.OnSwitchToTrack = func() { gateReset = true }
	s.switchTo(VideoTransportTrack)
	if !gateReset {
		t.Error("OnSwitchToTrack not called")
	}
	dc.take() // ack goes through SendAck, not dc; drain anyway
	s.handleFeedback(fragNackMsg(h2.Epoch, h2.FrameID, 0))
	if got := dc.take(); len(got) != 0 {
		t.Fatalf("track mode: got %d messages, want 0", len(got))
	}
}

// TestSwitchToWebCodecsNeedsH264Cap checks auto mode refuses a webcodecs switch
// from a viewer whose HELLO lacks H.264 support.
func TestSwitchToWebCodecsNeedsH264Cap(t *testing.T) {
	s := newVideoSender(videoSenderConfig{Mode: VideoTransportAuto})
	s.setChannel(&fakeDC{state: webrtc.DataChannelStateOpen})
	s.handleFeedback(helloMsg(0))
	if got, _ := s.switchTo(VideoTransportWebCodecs); got != VideoTransportTrack {
		t.Fatalf("active = %q, want track", got)
	}
}
