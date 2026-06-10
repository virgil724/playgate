package rtc

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/playgate/playgate-host/internal/core"
)

// InputCommand wire format (the "input" DataChannel payload).
//
// A controller state snapshot is encoded as a fixed-size, 13-byte, little-endian
// binary frame. It is deliberately compact so it fits comfortably in a single
// SCTP message at 60 Hz, and self-describing enough (via a version byte) that the
// browser (T11) and host can evolve independently.
//
// Byte layout (offsets are zero-based, all multi-byte fields little-endian):
//
//	offset size field    type    description
//	------ ---- -------- ------- ------------------------------------------------
//	0      1    version  uint8   wire format version; must equal InputWireVersion
//	1      4    buttons  uint32  button bitmask (core.Button* constants)
//	5      2    lx       int16   left  stick X, fixed-point (see scaling below)
//	7      2    ly       int16   left  stick Y
//	9      2    rx       int16   right stick X
//	11     2    ry       int16   right stick Y
//	------ ---- -------- ------- ------------------------------------------------
//	total: 13 bytes
//
// Axis scaling: each float32 axis in [-1, 1] is mapped to int16 by multiplying by
// AxisScale (32767) and rounding to nearest, then clamped to [-32767, 32767].
// Decoding divides the int16 by AxisScale. Note -32768 is intentionally never
// produced so that encode/decode is symmetric around zero; a received -32768 is
// clamped to -1.0 on decode.
//
// Timestamp: the wire format carries NO timestamp. core.InputCommand.Timestamp is
// stamped on decode with the host's receive time (time.Now via the supplied
// clock). The DataChannel is unreliable/unordered, so a sender-side timestamp
// would add 8 bytes for little benefit; the host treats each snapshot as the
// latest-known desired state at receive time.
const (
	// InputWireVersion is the current wire format version byte (offset 0).
	InputWireVersion uint8 = 1

	// InputWireSize is the exact encoded length in bytes of one command.
	InputWireSize = 13

	// AxisScale is the fixed-point multiplier mapping a [-1,1] float axis to int16.
	AxisScale = 32767
)

// EncodeInputCommand serialises cmd into the 13-byte wire format described above.
// The Timestamp field is not encoded. The returned slice is freshly allocated.
func EncodeInputCommand(cmd core.InputCommand) []byte {
	buf := make([]byte, InputWireSize)
	buf[0] = InputWireVersion
	binary.LittleEndian.PutUint32(buf[1:5], cmd.Buttons)
	binary.LittleEndian.PutUint16(buf[5:7], uint16(axisToInt16(cmd.LX)))
	binary.LittleEndian.PutUint16(buf[7:9], uint16(axisToInt16(cmd.LY)))
	binary.LittleEndian.PutUint16(buf[9:11], uint16(axisToInt16(cmd.RX)))
	binary.LittleEndian.PutUint16(buf[11:13], uint16(axisToInt16(cmd.RY)))
	return buf
}

// DecodeInputCommand parses a wire-format frame into a core.InputCommand. The
// returned command's Timestamp is set to now. It returns an error if the buffer
// length or version byte is wrong.
func DecodeInputCommand(buf []byte, now time.Time) (core.InputCommand, error) {
	if len(buf) != InputWireSize {
		return core.InputCommand{}, fmt.Errorf("rtc: input frame must be %d bytes, got %d", InputWireSize, len(buf))
	}
	if buf[0] != InputWireVersion {
		return core.InputCommand{}, fmt.Errorf("rtc: unsupported input wire version %d (want %d)", buf[0], InputWireVersion)
	}
	return core.InputCommand{
		Buttons:   binary.LittleEndian.Uint32(buf[1:5]),
		LX:        int16ToAxis(int16(binary.LittleEndian.Uint16(buf[5:7]))),
		LY:        int16ToAxis(int16(binary.LittleEndian.Uint16(buf[7:9]))),
		RX:        int16ToAxis(int16(binary.LittleEndian.Uint16(buf[9:11]))),
		RY:        int16ToAxis(int16(binary.LittleEndian.Uint16(buf[11:13]))),
		Timestamp: now,
	}, nil
}

// axisToInt16 maps a [-1,1] float to a fixed-point int16, clamped and rounded.
func axisToInt16(v float32) int16 {
	scaled := float64(v) * AxisScale
	switch {
	case scaled > AxisScale:
		scaled = AxisScale
	case scaled < -AxisScale:
		scaled = -AxisScale
	}
	// Round to nearest, ties away from zero.
	if scaled >= 0 {
		return int16(scaled + 0.5)
	}
	return int16(scaled - 0.5)
}

// int16ToAxis maps a fixed-point int16 back to a [-1,1] float, clamping the
// asymmetric extreme -32768 to exactly -1.0.
func int16ToAxis(v int16) float32 {
	if v <= -AxisScale {
		return -1
	}
	return float32(v) / AxisScale
}
