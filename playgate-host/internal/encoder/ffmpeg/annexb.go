package ffmpeg

// H.264 Annex-B parsing.
//
// An Annex-B byte stream is a sequence of NAL units, each preceded by a start
// code: either 0x000001 (3-byte) or 0x00000001 (4-byte). The low 5 bits of the
// first byte after the start code give the NAL unit type.
//
// We group NAL units into access units (AUs). For our low-latency, no-B-frame
// stream an AU is "everything from one VCL slice's leading parameter sets up to
// (but not including) the next VCL slice". Practically we accumulate NAL units
// and cut an AU when we see the next slice NALU (type 1 or 5) after we have
// already seen one in the current AU. SPS/PPS that precede an IDR are folded
// into the keyframe AU so the packet is independently decodable.

// NAL unit types we care about (H.264 / ITU-T H.264 Table 7-1).
const (
	nalTypeNonIDR = 1 // coded slice of a non-IDR picture (P-frame here)
	nalTypeIDR    = 5 // coded slice of an IDR picture (keyframe)
	nalTypeSEI    = 6
	nalTypeSPS    = 7 // sequence parameter set
	nalTypePPS    = 8 // picture parameter set
	nalTypeAUD    = 9 // access unit delimiter
)

// nalUnit is one parsed NAL unit, located by its byte offsets within the source
// buffer (start code inclusive).
type nalUnit struct {
	// start is the offset of the NAL unit's start code within the source buffer.
	start int
	// end is the offset just past the NAL unit (exclusive).
	end int
	// nalType is the NAL unit type (low 5 bits of the first payload byte).
	nalType byte
}

// isVCL reports whether the NAL unit carries coded slice data (a picture).
func (n nalUnit) isVCL() bool {
	return n.nalType == nalTypeNonIDR || n.nalType == nalTypeIDR
}

// startCodeLen returns the length of the Annex-B start code at b[i:], or 0 if
// there is no start code at that position.
func startCodeLen(b []byte, i int) int {
	if i+3 <= len(b) && b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
		return 3
	}
	if i+4 <= len(b) && b[i] == 0 && b[i+1] == 0 && b[i+2] == 0 && b[i+3] == 1 {
		return 4
	}
	return 0
}

// findStartCode returns the index of the next start code at or after `from`,
// and its length. It returns (-1, 0) if none is found.
func findStartCode(b []byte, from int) (idx, scLen int) {
	for i := from; i+3 <= len(b); i++ {
		if l := startCodeLen(b, i); l != 0 {
			return i, l
		}
	}
	return -1, 0
}

// splitNALUs splits an Annex-B buffer into NAL units described by their byte
// offsets within buf. It assumes buf begins at a start code; bytes before the
// first start code are ignored.
func splitNALUs(buf []byte) []nalUnit {
	var out []nalUnit
	start, scLen := findStartCode(buf, 0)
	if start < 0 {
		return nil
	}
	for start >= 0 {
		payloadStart := start + scLen
		next, nextLen := findStartCode(buf, payloadStart)
		end := len(buf)
		if next >= 0 {
			end = next
		}
		if payloadStart < end {
			var t byte
			if payloadStart < len(buf) {
				t = buf[payloadStart] & 0x1f
			}
			out = append(out, nalUnit{start: start, end: end, nalType: t})
		}
		start, scLen = next, nextLen
	}
	return out
}

// accessUnit is a decode-complete group of NAL units forming one picture, plus
// any parameter sets/SEI that precede it.
type accessUnit struct {
	data       []byte
	isKeyframe bool
}

// AnnexBSplitter is a stateful, streaming splitter that turns a sequence of
// arbitrary Annex-B byte chunks (as read from ffmpeg stdout) into complete
// access units. It buffers a partial trailing NAL unit across chunks and emits
// an AU only once the *next* picture's first slice is seen, guaranteeing each
// emitted AU is complete.
type AnnexBSplitter struct {
	buf []byte // unconsumed bytes (always starts at a start code once primed)
}

// NewAnnexBSplitter returns a ready-to-use splitter.
func NewAnnexBSplitter() *AnnexBSplitter { return &AnnexBSplitter{} }

// Push appends a chunk and returns any access units that became complete. The
// returned AUs own their data (copied out of the internal buffer), so callers
// may retain them freely.
func (s *AnnexBSplitter) Push(chunk []byte) []accessUnit {
	s.buf = append(s.buf, chunk...)
	return s.drain(false)
}

// Flush returns the final access unit held in the buffer at end-of-stream.
// After Flush the splitter is empty.
func (s *AnnexBSplitter) Flush() []accessUnit {
	return s.drain(true)
}

// drain extracts complete access units from the buffer. When final is false it
// retains the last (in-progress) AU in the buffer so it can be completed by a
// later chunk. When final is true it emits everything remaining.
//
// AU boundary rule: an AU is closed when we encounter a NALU that begins a new
// picture, i.e. a VCL slice (or an explicit AUD) after the current AU already
// contains a VCL slice. Parameter sets / SEI / AUD that precede a picture are
// folded into that picture's AU, so an IDR AU carries its own SPS+PPS and is
// independently decodable.
func (s *AnnexBSplitter) drain(final bool) []accessUnit {
	nalus := splitNALUs(s.buf)
	if len(nalus) == 0 {
		return nil
	}

	var (
		aus        []accessUnit
		auStart    = nalus[0].start
		seenVCL    bool
		auHasIDR   bool
		emittedEnd int // byte offset up to which we've emitted complete AUs
	)

	for _, n := range nalus {
		startsNewAU := seenVCL && (n.isVCL() || n.nalType == nalTypeAUD)
		if startsNewAU {
			aus = append(aus, accessUnit{
				data:       cloneBytes(s.buf[auStart:n.start]),
				isKeyframe: auHasIDR,
			})
			emittedEnd = n.start
			auStart = n.start
			seenVCL = false
			auHasIDR = false
		}
		if n.isVCL() {
			seenVCL = true
			if n.nalType == nalTypeIDR {
				auHasIDR = true
			}
		}
	}

	if final {
		if auStart < len(s.buf) {
			aus = append(aus, accessUnit{
				data:       cloneBytes(s.buf[auStart:]),
				isKeyframe: auHasIDR,
			})
		}
		s.buf = nil
		return aus
	}

	// Drop the bytes we've already emitted; keep the in-progress AU buffered.
	if emittedEnd > 0 {
		s.buf = append(s.buf[:0:0], s.buf[emittedEnd:]...)
	}
	return aus
}

// cloneBytes returns an independent copy of b.
func cloneBytes(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
