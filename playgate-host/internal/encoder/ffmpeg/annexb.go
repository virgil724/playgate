package ffmpeg

// H.264 Annex-B parsing.
//
// An Annex-B byte stream is a sequence of NAL units, each preceded by a start
// code: either 0x000001 (3-byte) or 0x00000001 (4-byte). The low 5 bits of the
// first byte after the start code give the NAL unit type.
//
// We group NAL units into access units (AUs). An AU contains all NAL units that
// belong to a single coded picture: parameter sets and SEI that precede it, plus
// every VCL slice of that picture. A multi-slice frame (e.g. libx264 with
// sliced-threads) produces multiple VCL NALUs per picture; they must all be
// grouped into the same AU or the decoder sees incomplete frames (green screen).
//
// To distinguish "continuation slice of the same picture" from "first slice of
// the next picture" we check first_mb_in_slice, the first syntax element of the
// slice header (H.264 §7.3.3). It is unsigned exp-Golomb coded: value 0 encodes
// as a single '1' bit, so if the MSB of the first slice-header byte is 1, the
// slice starts at macroblock 0 (= new picture). If it is 0, the slice starts at
// a later macroblock (= continuation of the current picture).

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
	// firstSlice is true for VCL NAL units (type 1 or 5) where
	// first_mb_in_slice == 0, meaning this is the first slice of a new coded
	// picture. Continuation slices of the same picture have firstSlice == false.
	// For non-VCL NALUs this field is meaningless.
	firstSlice bool
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
			fs := false
			if t == nalTypeNonIDR || t == nalTypeIDR {
				// Check first_mb_in_slice: the first syntax element in the
				// slice header (the byte immediately after the 1-byte NAL
				// header). Value 0 encodes as a single '1' bit in unsigned
				// exp-Golomb, so MSB=1 means first_mb_in_slice==0 → this
				// slice starts a new picture. MSB=0 means first_mb_in_slice>0
				// → continuation slice of the current picture.
				sliceStart := payloadStart + 1
				if sliceStart < end {
					fs = buf[sliceStart]&0x80 != 0
				} else {
					// Can't read slice header byte yet (incomplete NALU at
					// buffer end). Default to false (assume continuation) so
					// we don't prematurely cut the AU. The trailing NALU will
					// be re-parsed once more data arrives.
					fs = false
				}
			}
			out = append(out, nalUnit{start: start, end: end, nalType: t, firstSlice: fs})
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
// picture — either the first VCL slice of a new picture (first_mb_in_slice == 0)
// or an explicit AUD — provided the current AU already contains a VCL slice.
// Continuation slices (first_mb_in_slice > 0) of a multi-slice frame are kept
// in the same AU. Parameter sets / SEI that precede a picture are folded into
// that picture's AU, so an IDR AU carries its own SPS+PPS and is independently
// decodable.
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
		// A new AU starts when we already have a VCL slice and encounter:
		//   - a VCL NALU that is the first slice of a new picture, OR
		//   - an explicit access-unit delimiter.
		// Continuation slices (firstSlice==false) stay in the current AU.
		startsNewAU := seenVCL && (n.nalType == nalTypeAUD || (n.isVCL() && n.firstSlice))
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
