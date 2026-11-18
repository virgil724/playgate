package rtc

import (
	"bytes"
	"testing"

	"github.com/pion/webrtc/v4"
)

func TestSenderFECGatedByHello(t *testing.T) {
	au := testAU(3000, true) // k=3 at the default stride
	cases := []struct {
		name      string
		fec       int
		caps      uint8
		maxFEC    uint8
		wantTotal int // messages per frame
	}{
		{"off", FECNone, CapWebCodecsH264 | CapFECRS, 255, 3},
		{"rs without cap", FECRS, CapWebCodecsH264 | CapFECXOR, 255, 3},
		{"xor without cap", FECXOR, CapWebCodecsH264 | CapFECRS, 255, 3},
		{"rs with cap", FECRS, CapWebCodecsH264 | CapFECRS, 255, 5},
		{"xor with cap", FECXOR, CapWebCodecsH264 | CapFECXOR, 255, 5},
		{"max_fec caps parity", FECRS, CapWebCodecsH264 | CapFECRS, 1, 4},
		{"max_fec 0 means none", FECRS, CapWebCodecsH264 | CapFECRS, 0, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dc := &fakeDC{state: webrtc.DataChannelStateOpen}
			s := newVideoSender(videoSenderConfig{
				Mode: VideoTransportWebCodecs, BufferMax: 1 << 20, IDRPaceMS: -1,
				FEC: c.fec, FECRatio: 0.5,
			})
			s.setChannel(dc)
			hm := helloMsg(c.caps)
			hm[5] = c.maxFEC
			s.handleFeedback(hm)
			_ = s.writeFrame(pkt(au, 0, true))
			msgs := dc.take()
			if len(msgs) != c.wantTotal {
				t.Fatalf("got %d messages, want %d", len(msgs), c.wantTotal)
			}
			if c.wantTotal > 3 {
				got, err := fecReassemble(t, msgs, map[int]bool{1: true})
				if err != nil || !bytes.Equal(got, au) {
					t.Fatalf("recover: err=%v", err)
				}
			}
		})
	}
}
