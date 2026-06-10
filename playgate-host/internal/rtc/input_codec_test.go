package rtc

import (
	"math"
	"testing"
	"time"

	"github.com/playgate/playgate-host/internal/core"
)

func TestEncodeInputCommandLayout(t *testing.T) {
	cmd := core.InputCommand{
		Buttons: core.ButtonA | core.ButtonDpadUp,
		LX:      1, LY: -1, RX: 0, RY: 0.5,
	}
	buf := EncodeInputCommand(cmd)
	if len(buf) != InputWireSize {
		t.Fatalf("encoded length = %d, want %d", len(buf), InputWireSize)
	}
	if buf[0] != InputWireVersion {
		t.Fatalf("version byte = %d, want %d", buf[0], InputWireVersion)
	}
	// buttons little-endian at offset 1.
	wantButtons := uint32(core.ButtonA | core.ButtonDpadUp)
	gotButtons := uint32(buf[1]) | uint32(buf[2])<<8 | uint32(buf[3])<<16 | uint32(buf[4])<<24
	if gotButtons != wantButtons {
		t.Fatalf("buttons = %#x, want %#x", gotButtons, wantButtons)
	}
}

func TestInputCommandRoundTrip(t *testing.T) {
	now := time.Unix(1700000000, 0)
	cases := []core.InputCommand{
		{},
		{Buttons: 0xFFFFFFFF, LX: 1, LY: 1, RX: 1, RY: 1},
		{Buttons: core.ButtonB | core.ButtonZR, LX: -1, LY: -1, RX: -1, RY: -1},
		{LX: 0.25, LY: -0.5, RX: 0.75, RY: -0.123},
	}
	for i, in := range cases {
		buf := EncodeInputCommand(in)
		out, err := DecodeInputCommand(buf, now)
		if err != nil {
			t.Fatalf("case %d: decode: %v", i, err)
		}
		if out.Buttons != in.Buttons {
			t.Errorf("case %d: buttons = %#x, want %#x", i, out.Buttons, in.Buttons)
		}
		if !out.Timestamp.Equal(now) {
			t.Errorf("case %d: timestamp = %v, want %v", i, out.Timestamp, now)
		}
		const eps = 1.0 / AxisScale // one quantisation step
		for _, p := range []struct {
			name     string
			in, out  float32
		}{
			{"LX", in.LX, out.LX},
			{"LY", in.LY, out.LY},
			{"RX", in.RX, out.RX},
			{"RY", in.RY, out.RY},
		} {
			if math.Abs(float64(p.in-p.out)) > eps {
				t.Errorf("case %d: %s = %v, want ~%v (eps %v)", i, p.name, p.out, p.in, eps)
			}
		}
	}
}

func TestDecodeInputCommandErrors(t *testing.T) {
	now := time.Now()
	if _, err := DecodeInputCommand(make([]byte, InputWireSize-1), now); err == nil {
		t.Error("short buffer: expected error")
	}
	if _, err := DecodeInputCommand(make([]byte, InputWireSize+1), now); err == nil {
		t.Error("long buffer: expected error")
	}
	bad := EncodeInputCommand(core.InputCommand{})
	bad[0] = 99 // wrong version
	if _, err := DecodeInputCommand(bad, now); err == nil {
		t.Error("bad version: expected error")
	}
}

func TestAxisClamp(t *testing.T) {
	// Out-of-range floats must clamp to +-1 after round-trip.
	in := core.InputCommand{LX: 2.5, LY: -3.0}
	out, err := DecodeInputCommand(EncodeInputCommand(in), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if out.LX != 1 {
		t.Errorf("LX clamp = %v, want 1", out.LX)
	}
	if out.LY != -1 {
		t.Errorf("LY clamp = %v, want -1", out.LY)
	}
}
