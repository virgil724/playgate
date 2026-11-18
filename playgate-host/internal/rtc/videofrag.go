package rtc

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// WebCodecs video transport wire format (docs/webcodecs-transport.md section 3).
// All integers are big-endian.

// Channel labels for the WebCodecs transport.
const (
	// VideoChannelLabel is the unreliable/unordered channel carrying frame fragments.
	VideoChannelLabel = "video"
	// VideoFeedbackChannelLabel is the reliable channel carrying viewer feedback.
	VideoFeedbackChannelLabel = "videofb"
)

// Header constants.
const (
	// VideoHeaderSize is the fixed fragment header length in bytes.
	VideoHeaderSize = 28
	// VideoHeaderVersion is the only defined header version.
	VideoHeaderVersion = 1

	// DefaultVideoFragmentBytes is the default total message size (header + payload).
	DefaultVideoFragmentBytes = 1200
	// MinVideoFragmentBytes / MaxVideoFragmentBytes bound the configurable size.
	MinVideoFragmentBytes = 600
	MaxVideoFragmentBytes = 1400
)

// Fragment flag bits.
const (
	FlagKey    = 1 << 0 // IDR access unit
	FlagParam  = 1 << 1 // access unit starts with SPS+PPS
	FlagParity = 1 << 2 // FEC parity fragment (T13)
)

// CodecH264 is the only defined codec value (H.264 Annex-B).
const CodecH264 = 0

// VideoHeader is the decoded 28-byte fragment header.
type VideoHeader struct {
	Version     uint8
	Flags       uint8
	Epoch       uint16
	FrameID     uint32
	PTSUs       uint32
	FrameSize   uint32
	FragIdx     uint16
	DataCount   uint16
	ParityCount uint16
	Codec       uint8
	FECScheme   uint8
	SendMS      uint32
}

// Encode writes the header into dst, which must be at least VideoHeaderSize long.
func (h VideoHeader) Encode(dst []byte) {
	_ = dst[VideoHeaderSize-1]
	dst[0] = h.Version
	dst[1] = h.Flags
	binary.BigEndian.PutUint16(dst[2:], h.Epoch)
	binary.BigEndian.PutUint32(dst[4:], h.FrameID)
	binary.BigEndian.PutUint32(dst[8:], h.PTSUs)
	binary.BigEndian.PutUint32(dst[12:], h.FrameSize)
	binary.BigEndian.PutUint16(dst[16:], h.FragIdx)
	binary.BigEndian.PutUint16(dst[18:], h.DataCount)
	binary.BigEndian.PutUint16(dst[20:], h.ParityCount)
	dst[22] = h.Codec
	dst[23] = h.FECScheme
	binary.BigEndian.PutUint32(dst[24:], h.SendMS)
}

// ErrShortHeader is returned when a message is shorter than VideoHeaderSize.
var ErrShortHeader = errors.New("rtc: video fragment shorter than header")

// ParseVideoHeader decodes a header and returns it with the payload slice.
func ParseVideoHeader(msg []byte) (VideoHeader, []byte, error) {
	if len(msg) < VideoHeaderSize {
		return VideoHeader{}, nil, ErrShortHeader
	}
	h := VideoHeader{
		Version:     msg[0],
		Flags:       msg[1],
		Epoch:       binary.BigEndian.Uint16(msg[2:]),
		FrameID:     binary.BigEndian.Uint32(msg[4:]),
		PTSUs:       binary.BigEndian.Uint32(msg[8:]),
		FrameSize:   binary.BigEndian.Uint32(msg[12:]),
		FragIdx:     binary.BigEndian.Uint16(msg[16:]),
		DataCount:   binary.BigEndian.Uint16(msg[18:]),
		ParityCount: binary.BigEndian.Uint16(msg[20:]),
		Codec:       msg[22],
		FECScheme:   msg[23],
		SendMS:      binary.BigEndian.Uint32(msg[24:]),
	}
	if h.Version != VideoHeaderVersion {
		return h, nil, fmt.Errorf("rtc: unknown video header version %d", h.Version)
	}
	return h, msg[VideoHeaderSize:], nil
}

// FrameMeta carries the per-frame header fields shared by every fragment.
type FrameMeta struct {
	Flags   uint8
	Epoch   uint16
	FrameID uint32
	PTSUs   uint32
	SendMS  uint32
}

// FragmentCount returns k = ceil(frameSize/stride).
func FragmentCount(frameSize, stride int) int {
	if frameSize <= 0 || stride <= 0 {
		return 0
	}
	return (frameSize + stride - 1) / stride
}

// FragmentFrame splits one Annex-B access unit into wire messages (header +
// payload), fragment 0 first. stride is the payload bytes per data fragment
// (message size minus VideoHeaderSize). It returns an error for an empty frame
// or one needing more than 65535 fragments. No FEC parity is produced (m=0).
func FragmentFrame(meta FrameMeta, au []byte, stride int) ([][]byte, error) {
	if stride <= 0 {
		return nil, errors.New("rtc: fragment stride must be positive")
	}
	if len(au) == 0 {
		return nil, errors.New("rtc: empty access unit")
	}
	k := FragmentCount(len(au), stride)
	if k > 65535 {
		return nil, fmt.Errorf("rtc: frame of %d bytes needs %d fragments", len(au), k)
	}
	out := make([][]byte, 0, k)
	h := VideoHeader{
		Version:   VideoHeaderVersion,
		Flags:     meta.Flags,
		Epoch:     meta.Epoch,
		FrameID:   meta.FrameID,
		PTSUs:     meta.PTSUs,
		FrameSize: uint32(len(au)),
		DataCount: uint16(k),
		Codec:     CodecH264,
		SendMS:    meta.SendMS,
	}
	for i := 0; i < k; i++ {
		lo := i * stride
		hi := lo + stride
		if hi > len(au) {
			hi = len(au)
		}
		msg := make([]byte, VideoHeaderSize+hi-lo)
		h.FragIdx = uint16(i)
		h.Encode(msg)
		copy(msg[VideoHeaderSize:], au[lo:hi])
		out = append(out, msg)
	}
	return out, nil
}

// --- Annex-B helpers ---

// nalSpan locates one NAL unit (start code inclusive) in an Annex-B buffer.
type nalSpan struct {
	typ        byte
	start, end int
}

func scanNALs(b []byte) []nalSpan {
	var spans []nalSpan
	i := 0
	for i+3 <= len(b) {
		sc := 0
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			sc = 3
		} else if i+4 <= len(b) && b[i] == 0 && b[i+1] == 0 && b[i+2] == 0 && b[i+3] == 1 {
			sc = 4
		}
		if sc == 0 {
			i++
			continue
		}
		if len(spans) > 0 {
			spans[len(spans)-1].end = i
		}
		if i+sc < len(b) {
			spans = append(spans, nalSpan{typ: b[i+sc] & 0x1f, start: i, end: len(b)})
		}
		i += sc
	}
	return spans
}

// paramSets extracts the SPS and PPS NAL units (with start codes) from an
// Annex-B access unit, concatenated in stream order, and reports whether each
// kind was present.
func paramSets(au []byte) (ps []byte, hasSPS, hasPPS bool) {
	for _, n := range scanNALs(au) {
		switch n.typ {
		case 7:
			hasSPS = true
			ps = append(ps, au[n.start:n.end]...)
		case 8:
			hasPPS = true
			ps = append(ps, au[n.start:n.end]...)
		}
	}
	return ps, hasSPS, hasPPS
}
