package gateway

import (
	"bytes"
	"testing"

	"github.com/playgate/playgate-host/internal/core"
)

func TestCRC16CheckValue(t *testing.T) {
	if got := crc16([]byte("123456789")); got != 0x29B1 {
		t.Fatalf("crc16 = %#04x, want 0x29b1 (CCITT-FALSE check value)", got)
	}
}

// TestEncodeInputGolden checks A pressed, left stick fully up, seq 1 against a
// vector computed independently (Python reference COBS/CRC).
func TestEncodeInputGolden(t *testing.T) {
	want := []byte{
		0x06, 0x01, 0x01, 0x11, 0x02, 0x01, 0x01, 0x01, 0x01, 0x01, 0x03, 0x01,
		0x80, 0x01, 0x01, 0x01, 0x02, 0x01, 0x01, 0x01, 0x03, 0x79, 0x30, 0x00,
	}
	got := EncodeInput(core.InputCommand{Buttons: core.ButtonA, LY: -1}, 1)
	if !bytes.Equal(got, want) {
		t.Fatalf("frame\n got % x\nwant % x", got, want)
	}
}

func TestCOBSRoundTrip(t *testing.T) {
	long := bytes.Repeat([]byte{7}, 300) // crosses the 254-byte block limit
	for _, in := range [][]byte{
		{}, {0}, {0, 0}, {1, 0, 2}, {1, 2, 3}, long, append(long[:254:254], 0, 5),
	} {
		enc := cobsEncode(in)
		if bytes.IndexByte(enc, 0) >= 0 {
			t.Fatalf("encoding of % x contains 0x00", in)
		}
		dec, err := cobsDecode(enc)
		if err != nil || !bytes.Equal(dec, in) {
			t.Fatalf("round trip of %d bytes: got %d bytes, err %v", len(in), len(dec), err)
		}
	}
}

func TestDecodeFrameRoundTripAndErrors(t *testing.T) {
	cmd := core.InputCommand{Buttons: core.ButtonDpadRight | core.ButtonZR, LX: 0.5, RY: 1}
	wire := EncodeInput(cmd, 0xDEADBEEF)
	typ, p, err := DecodeFrame(wire[:len(wire)-1])
	if err != nil || typ != typeInput {
		t.Fatalf("decode: type %#x err %v", typ, err)
	}
	if !bytes.Equal(p, inputPayload(cmd, 0xDEADBEEF)) {
		t.Fatalf("payload mismatch: % x", p)
	}

	bad := append([]byte(nil), wire[:len(wire)-1]...)
	bad[len(bad)-2] ^= 0x01 // flip a CRC bit
	if _, _, err := DecodeFrame(bad); err == nil {
		t.Error("corrupt CRC accepted")
	}
	if _, _, err := DecodeFrame([]byte{0x02, 0x01}); err == nil {
		t.Error("short frame accepted")
	}
}

func TestAxisClampAndRound(t *testing.T) {
	for _, c := range []struct {
		in   float32
		want int16
	}{{0, 0}, {1, 32767}, {-1, -32767}, {2, 32767}, {-2, -32767}, {0.5, 16384}} {
		if got := axis(c.in); got != c.want {
			t.Errorf("axis(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseStatus(t *testing.T) {
	p := []byte{
		1, HIDConnected, 1, 0x04, // version, hid, source, flags (timeout_neutral)
		0x10, 0, 0, 0, // last seq 16
		5, 0, 0, 0, 2, 0, 0, 0, 1, 0, // accepted 5, invalid 0, stale 2, conflicts 0, superseded 1
		0x1C, 0x17, // 5916 us
	}
	s, err := ParseStatus(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.HIDState != HIDConnected || s.LastSeq != 16 || s.Accepted != 5 || s.Stale != 2 ||
		s.Superseded != 1 || s.ReceiveToSubmitUS != 5916 || !s.TimeoutNeutral() {
		t.Fatalf("got %+v", s)
	}
	if _, err := ParseStatus(p[:19]); err == nil {
		t.Error("short status accepted")
	}
}
