package rtc

import (
	"encoding/binary"
	"fmt"
)

// Viewer to host feedback on the reliable "videofb" channel
// (docs/webcodecs-transport.md section 4).

// Feedback message types.
const (
	FBKeyframeRequest = 0x01
	FBFrameReport     = 0x02
	FBHello           = 0x03
	FBFragNack        = 0x06
)

// Feedback message sizes. Messages are fixed-size per type; longer ones are
// accepted and the tail ignored for forward compatibility.
const (
	fbVersion           = 1
	KeyframeRequestSize = 16
	FrameReportSize     = 24
	HelloSize           = 12
	FragNackMinSize     = 12
	// FragNackMaxIndices bounds the index list of one FRAG_NACK.
	FragNackMaxIndices = 255
)

// HELLO capability bits.
const (
	CapWebCodecsH264 = 1 << 0
	CapHardware      = 1 << 1
	CapAnnexB        = 1 << 2
	CapFECXOR        = 1 << 3
	CapFECRS         = 1 << 4
	CapFragNack      = 1 << 5
)

// KeyframeRequest reasons.
const (
	ReasonUnknown      = 0
	ReasonFrameLost    = 1
	ReasonDecoderError = 2
	ReasonQueueBacklog = 3
	ReasonInitialJoin  = 4
	ReasonWaitingEpoch = 5
)

// KeyframeRequest is the decoded KEYFRAME_REQUEST message.
type KeyframeRequest struct {
	Epoch      uint16
	Reason     uint8
	LastGoodID uint32
	LostID     uint32
}

// FrameReport is the decoded FRAME_REPORT message.
type FrameReport struct {
	HighestID      uint32
	FramesComplete uint16
	FramesLost     uint16
	FragsRecv      uint16
	FragsExpected  uint16
	Recovered      uint16
	DecodeQueue    uint16
	JitterMSx10    uint16
	DecMSx10       uint16
}

// Hello is the decoded HELLO message.
type Hello struct {
	Caps     uint8
	MaxFEC   uint8
	ReportMS uint16
}

// FragNack is the decoded FRAG_NACK message: the viewer asks for fragments of
// one frame again. An empty Indices list means "every fragment of the frame"
// (none of it arrived, so the viewer does not know its fragment count).
type FragNack struct {
	Epoch   uint16
	FrameID uint32
	Indices []uint16
}

// ParseFeedback decodes one videofb message. It returns (nil, nil) for an
// unknown version or type (the receiver must ignore it and never close the
// channel), and an error for a known type with a truncated body. The returned
// value is a KeyframeRequest, FrameReport, Hello or FragNack.
func ParseFeedback(b []byte) (any, error) {
	if len(b) < 4 || b[0] != fbVersion {
		return nil, nil
	}
	switch b[1] {
	case FBKeyframeRequest:
		if len(b) < KeyframeRequestSize {
			return nil, fmt.Errorf("rtc: KEYFRAME_REQUEST too short (%d)", len(b))
		}
		return KeyframeRequest{
			Epoch:      binary.BigEndian.Uint16(b[4:]),
			Reason:     b[6],
			LastGoodID: binary.BigEndian.Uint32(b[8:]),
			LostID:     binary.BigEndian.Uint32(b[12:]),
		}, nil
	case FBFrameReport:
		if len(b) < FrameReportSize {
			return nil, fmt.Errorf("rtc: FRAME_REPORT too short (%d)", len(b))
		}
		return FrameReport{
			HighestID:      binary.BigEndian.Uint32(b[4:]),
			FramesComplete: binary.BigEndian.Uint16(b[8:]),
			FramesLost:     binary.BigEndian.Uint16(b[10:]),
			FragsRecv:      binary.BigEndian.Uint16(b[12:]),
			FragsExpected:  binary.BigEndian.Uint16(b[14:]),
			Recovered:      binary.BigEndian.Uint16(b[16:]),
			DecodeQueue:    binary.BigEndian.Uint16(b[18:]),
			JitterMSx10:    binary.BigEndian.Uint16(b[20:]),
			DecMSx10:       binary.BigEndian.Uint16(b[22:]),
		}, nil
	case FBHello:
		if len(b) < HelloSize {
			return nil, fmt.Errorf("rtc: HELLO too short (%d)", len(b))
		}
		return Hello{Caps: b[4], MaxFEC: b[5], ReportMS: binary.BigEndian.Uint16(b[6:])}, nil
	case FBFragNack:
		if len(b) < FragNackMinSize {
			return nil, fmt.Errorf("rtc: FRAG_NACK too short (%d)", len(b))
		}
		count := int(binary.BigEndian.Uint16(b[6:]))
		if count > FragNackMaxIndices || len(b) < FragNackMinSize+2*count {
			return nil, fmt.Errorf("rtc: FRAG_NACK bad count %d for %d bytes", count, len(b))
		}
		n := FragNack{Epoch: binary.BigEndian.Uint16(b[4:]), FrameID: binary.BigEndian.Uint32(b[8:])}
		for i := 0; i < count; i++ {
			n.Indices = append(n.Indices, binary.BigEndian.Uint16(b[FragNackMinSize+2*i:]))
		}
		return n, nil
	}
	return nil, nil
}

// epochOlder reports whether a is strictly older than b in 16-bit serial
// number arithmetic.
func epochOlder(a, b uint16) bool { return int16(a-b) < 0 }
