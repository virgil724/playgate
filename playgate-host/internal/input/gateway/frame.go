package gateway

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/playgate/playgate-host/internal/core"
)

// Wire format of the PlayGate Switch 2 gateway's serial link (firmware:
// components/input_transports/serial_parser.c and controller_core.c):
//
//	decoded frame: [version=1][type][len][payload][crc_lo][crc_hi]
//	on the wire:   COBS(decoded frame) followed by a 0x00 delimiter
//
// The CRC is CRC-16/CCITT-FALSE over version, type, len and payload.
const (
	frameVersion = 1
	typeInput    = 0x01
	typeStatus   = 0x81

	inputVersion = 2
	inputSize    = 17
	statusSize   = 20

	statusVersion = 1

	buttonMask = 0x0003FFFF
	axisMax    = 32767
)

// HID link states reported in Status.HIDState.
const (
	HIDIdle         = 0
	HIDDiscoverable = 1
	HIDConnecting   = 2
	HIDConnected    = 3
)

var (
	errCOBS     = errors.New("gateway: malformed COBS frame")
	errFrame    = errors.New("gateway: malformed frame")
	errFrameCRC = errors.New("gateway: frame CRC mismatch")
)

// crc16 computes CRC-16/CCITT-FALSE (poly 0x1021, init 0xFFFF).
func crc16(b []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, v := range b {
		crc ^= uint16(v) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// cobsEncode returns the COBS encoding of in (without the 0x00 delimiter).
func cobsEncode(in []byte) []byte {
	out := make([]byte, 1, len(in)+len(in)/254+2)
	codeIdx, code := 0, byte(1)
	for _, v := range in {
		if v == 0 {
			out[codeIdx] = code
			codeIdx, code = len(out), 1
			out = append(out, 0)
			continue
		}
		out = append(out, v)
		code++
		if code == 0xFF {
			out[codeIdx] = code
			codeIdx, code = len(out), 1
			out = append(out, 0)
		}
	}
	out[codeIdx] = code
	return out
}

// cobsDecode reverses cobsEncode, matching the firmware decoder exactly.
func cobsDecode(in []byte) ([]byte, error) {
	out := make([]byte, 0, len(in))
	for i := 0; i < len(in); {
		code := int(in[i])
		i++
		if code == 0 || code-1 > len(in)-i {
			return nil, errCOBS
		}
		out = append(out, in[i:i+code-1]...)
		i += code - 1
		if code != 0xFF && i < len(in) {
			out = append(out, 0)
		}
	}
	return out, nil
}

// axis maps a normalised [-1, 1] axis to the gateway's -32767..32767. Both use
// negative Y for up, so no sign change is needed.
func axis(v float32) int16 {
	if v != v { // NaN
		return 0
	}
	f := math.Round(float64(v) * axisMax)
	return int16(max(-axisMax, min(axisMax, f)))
}

// inputPayload builds the 17-byte input payload.
func inputPayload(cmd core.InputCommand, seq uint32) []byte {
	p := make([]byte, inputSize)
	p[0] = inputVersion
	binary.LittleEndian.PutUint32(p[1:], cmd.Buttons&buttonMask)
	binary.LittleEndian.PutUint16(p[5:], uint16(axis(cmd.LX)))
	binary.LittleEndian.PutUint16(p[7:], uint16(axis(cmd.LY)))
	binary.LittleEndian.PutUint16(p[9:], uint16(axis(cmd.RX)))
	binary.LittleEndian.PutUint16(p[11:], uint16(axis(cmd.RY)))
	binary.LittleEndian.PutUint32(p[13:], seq)
	return p
}

// encodeFrame wraps payload into a delimited serial frame ready to write.
func encodeFrame(typ byte, payload []byte) []byte {
	dec := make([]byte, 0, 3+len(payload)+2)
	dec = append(dec, frameVersion, typ, byte(len(payload)))
	dec = append(dec, payload...)
	dec = binary.LittleEndian.AppendUint16(dec, crc16(dec))
	return append(cobsEncode(dec), 0)
}

// EncodeInput returns the wire bytes (COBS frame plus delimiter) for cmd with
// the given sequence number.
func EncodeInput(cmd core.InputCommand, seq uint32) []byte {
	return encodeFrame(typeInput, inputPayload(cmd, seq))
}

// DecodeFrame decodes one COBS frame (delimiter already stripped) and returns
// its type and payload after checking version, length and CRC.
func DecodeFrame(enc []byte) (typ byte, payload []byte, err error) {
	dec, err := cobsDecode(enc)
	if err != nil {
		return 0, nil, err
	}
	if len(dec) < 5 || dec[0] != frameVersion {
		return 0, nil, errFrame
	}
	n := int(dec[2])
	if len(dec) != 3+n+2 {
		return 0, nil, errFrame
	}
	if crc16(dec[:3+n]) != binary.LittleEndian.Uint16(dec[3+n:]) {
		return 0, nil, errFrameCRC
	}
	return dec[1], dec[3 : 3+n], nil
}

// Status is the gateway's 20-byte status report. Counters saturate at 65535.
type Status struct {
	HIDState     uint8
	ActiveSource uint8 // 0 none, 1 serial, 2 udp/websocket, 3 ble
	Flags        uint8 // bit0 wifi, bit1 ble, bit2 timeout_neutral
	LastSeq      uint32
	Accepted     uint16
	Invalid      uint16
	Stale        uint16
	Conflicts    uint16
	Superseded   uint16
	// ReceiveToSubmitUS is the device-local time from receiving the last
	// accepted frame to submitting its HID report. It can hold an old value
	// while idle.
	ReceiveToSubmitUS uint16
}

// TimeoutNeutral reports whether the gateway neutralised the controller
// because its input source went quiet.
func (s Status) TimeoutNeutral() bool { return s.Flags&4 != 0 }

// ParseStatus decodes a status payload.
func ParseStatus(p []byte) (Status, error) {
	if len(p) != statusSize || p[0] != statusVersion {
		return Status{}, fmt.Errorf("gateway: bad status payload (len %d)", len(p))
	}
	le := binary.LittleEndian
	return Status{
		HIDState:          p[1],
		ActiveSource:      p[2],
		Flags:             p[3],
		LastSeq:           le.Uint32(p[4:]),
		Accepted:          le.Uint16(p[8:]),
		Invalid:           le.Uint16(p[10:]),
		Stale:             le.Uint16(p[12:]),
		Conflicts:         le.Uint16(p[14:]),
		Superseded:        le.Uint16(p[16:]),
		ReceiveToSubmitUS: le.Uint16(p[18:]),
	}, nil
}
