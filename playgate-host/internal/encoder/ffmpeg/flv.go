package ffmpeg

// FLV demuxing.
//
// ffmpeg writes the encoder output as an FLV stream rather than a raw Annex-B
// byte stream. Raw Annex-B has no end-of-picture marker: an access unit is only
// known to be complete once the NEXT picture's first slice arrives, which holds
// every frame back by one frame interval. An FLV tag carries its payload length
// up front, so a picture is emitted the moment its last byte is read.
//
// Layout (all integers big-endian):
//
//	file header   "FLV" version(1) flags(1) headerSize(4)   then prevTagSize(4)
//	tag           type(1) dataSize(3) timestamp(3+1) streamID(3) data prevTagSize(4)
//	video data    frameType<<4|codecID(1) avcPacketType(1) compositionTime(3) body
//
// For AVC (codecID 7) avcPacketType 0 carries an AVCDecoderConfigurationRecord
// (SPS/PPS, NAL length size) and avcPacketType 1 carries one access unit as
// length-prefixed NAL units. Encoders that give ffmpeg no global header (no
// configuration record is ever written) have their Annex-B packets passed
// through verbatim, so the body is taken as Annex-B in that case.

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	flvHeaderMinSize  = 9
	flvTagHeaderSize  = 11
	flvPrevTagSize    = 4
	flvTagVideo       = 9
	flvCodecAVC       = 7
	flvAVCSeqHeader   = 0
	flvAVCNALU        = 1
	flvVideoBodyStart = 5
)

var annexBStartCode = []byte{0, 0, 0, 1}

// errFLVSync reports a stream that does not look like FLV. There is no way to
// resynchronise mid-stream, so the caller should restart the subprocess.
var errFLVSync = errors.New("encoder: output is not an FLV stream")

// FLVDemuxer is a stateful, streaming demuxer that turns arbitrary chunks of an
// FLV byte stream (as read from ffmpeg stdout) into H.264 access units in
// Annex-B format. Each access unit is returned as soon as its tag is complete.
type FLVDemuxer struct {
	buf        []byte // unconsumed bytes
	headerDone bool

	// From the AVCDecoderConfigurationRecord. haveConfig false means tag bodies
	// are already Annex-B.
	haveConfig bool
	nalLenSize int
	paramSets  []byte // SPS+PPS as Annex-B, prepended to IDR access units
}

// NewFLVDemuxer returns a ready-to-use demuxer.
func NewFLVDemuxer() *FLVDemuxer { return &FLVDemuxer{} }

// Push appends a chunk and returns any access units that became complete. The
// returned AUs own their data, so callers may retain them freely.
func (d *FLVDemuxer) Push(chunk []byte) ([]accessUnit, error) {
	d.buf = append(d.buf, chunk...)

	var aus []accessUnit
	pos := 0
	defer func() {
		// Drop consumed bytes; keep the in-progress tag buffered.
		if pos > 0 {
			d.buf = append(d.buf[:0:0], d.buf[pos:]...)
		}
	}()

	if !d.headerDone {
		if len(d.buf) < flvHeaderMinSize {
			return nil, nil
		}
		if string(d.buf[:3]) != "FLV" {
			return nil, errFLVSync
		}
		headerSize := int(binary.BigEndian.Uint32(d.buf[5:9]))
		if headerSize < flvHeaderMinSize {
			return nil, errFLVSync
		}
		if len(d.buf) < headerSize+flvPrevTagSize {
			return nil, nil
		}
		pos = headerSize + flvPrevTagSize
		d.headerDone = true
	}

	for {
		rest := d.buf[pos:]
		if len(rest) < flvTagHeaderSize {
			return aus, nil
		}
		dataSize := int(rest[1])<<16 | int(rest[2])<<8 | int(rest[3])
		total := flvTagHeaderSize + dataSize + flvPrevTagSize
		if len(rest) < total {
			return aus, nil
		}
		if rest[0] == flvTagVideo {
			au, ok, err := d.videoTag(rest[flvTagHeaderSize : flvTagHeaderSize+dataSize])
			if err != nil {
				return aus, err
			}
			if ok {
				aus = append(aus, au)
			}
		}
		pos += total
	}
}

// videoTag handles one video tag's data. It returns ok=false for tags that
// carry no picture (configuration record, end of sequence, non-AVC).
func (d *FLVDemuxer) videoTag(data []byte) (accessUnit, bool, error) {
	if len(data) < flvVideoBodyStart || data[0]&0x0f != flvCodecAVC {
		return accessUnit{}, false, nil
	}
	body := data[flvVideoBodyStart:]
	switch data[1] {
	case flvAVCSeqHeader:
		if err := d.parseConfig(body); err != nil {
			return accessUnit{}, false, err
		}
		return accessUnit{}, false, nil
	case flvAVCNALU:
		return d.accessUnit(body)
	default:
		return accessUnit{}, false, nil
	}
}

// parseConfig reads an AVCDecoderConfigurationRecord: the NAL length size used
// by subsequent tags and the SPS/PPS, which are kept in Annex-B form.
func (d *FLVDemuxer) parseConfig(rec []byte) error {
	if len(rec) < 6 {
		return fmt.Errorf("encoder: short AVC configuration record (%d bytes)", len(rec))
	}
	nalLenSize := int(rec[4]&0x03) + 1
	var sets []byte
	pos := 5
	// SPS count is the low 5 bits; PPS count is a full byte.
	for _, mask := range []byte{0x1f, 0xff} {
		if pos >= len(rec) {
			return errors.New("encoder: truncated AVC configuration record")
		}
		n := int(rec[pos] & mask)
		pos++
		for i := 0; i < n; i++ {
			if pos+2 > len(rec) {
				return errors.New("encoder: truncated AVC configuration record")
			}
			l := int(binary.BigEndian.Uint16(rec[pos:]))
			pos += 2
			if pos+l > len(rec) {
				return errors.New("encoder: truncated AVC configuration record")
			}
			sets = append(sets, annexBStartCode...)
			sets = append(sets, rec[pos:pos+l]...)
			pos += l
		}
	}
	d.haveConfig = true
	d.nalLenSize = nalLenSize
	d.paramSets = sets
	return nil
}

// accessUnit converts one tag body into an Annex-B access unit. An IDR that
// does not carry its own SPS gets the stored parameter sets prepended, so every
// keyframe is independently decodable (a late-joining viewer starts on one).
func (d *FLVDemuxer) accessUnit(body []byte) (accessUnit, bool, error) {
	var (
		out            []byte
		hasVCL, hasIDR bool
		hasSPS         bool
	)
	note := func(nalType byte) {
		switch nalType {
		case nalTypeIDR:
			hasIDR, hasVCL = true, true
		case nalTypeNonIDR:
			hasVCL = true
		case nalTypeSPS:
			hasSPS = true
		}
	}

	if !d.haveConfig {
		for _, n := range splitNALUs(body) {
			note(n.nalType)
		}
		out = body
	} else {
		out = make([]byte, 0, len(body)+len(d.paramSets)+16)
		for pos := 0; pos < len(body); {
			if pos+d.nalLenSize > len(body) {
				return accessUnit{}, false, errors.New("encoder: truncated NAL length in FLV tag")
			}
			l := 0
			for _, b := range body[pos : pos+d.nalLenSize] {
				l = l<<8 | int(b)
			}
			pos += d.nalLenSize
			if l > len(body)-pos {
				return accessUnit{}, false, errors.New("encoder: NAL unit overruns FLV tag")
			}
			if l > 0 {
				note(body[pos] & 0x1f)
				out = append(out, annexBStartCode...)
				out = append(out, body[pos:pos+l]...)
			}
			pos += l
		}
	}

	if !hasVCL {
		return accessUnit{}, false, nil
	}
	if hasIDR && !hasSPS && len(d.paramSets) > 0 {
		out = append(append(make([]byte, 0, len(d.paramSets)+len(out)), d.paramSets...), out...)
	} else if !d.haveConfig {
		out = cloneBytes(out) // body aliases the demuxer's buffer
	}
	return accessUnit{data: out, isKeyframe: hasIDR}, true, nil
}
