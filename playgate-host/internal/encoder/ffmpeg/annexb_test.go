package ffmpeg

import (
	"bytes"
	"testing"
)

// nal builds one NAL unit: a start code (3 or 4 bytes) followed by a payload
// whose first byte encodes the given NAL type in its low 5 bits.
//
// For VCL NALUs (type 1 or 5), the second byte's MSB encodes whether
// first_mb_in_slice == 0 (MSB=1 → first slice of a new picture) or > 0
// (MSB=0 → continuation slice). Callers must set this bit correctly.
func nal(scLen int, nalType byte, payload ...byte) []byte {
	var b []byte
	switch scLen {
	case 3:
		b = []byte{0, 0, 1}
	case 4:
		b = []byte{0, 0, 0, 1}
	default:
		panic("start code must be 3 or 4 bytes")
	}
	// NAL header byte: forbidden_zero_bit=0, nal_ref_idc in bits 5-6, type low 5.
	header := (nalType & 0x1f) | 0x60 // ref_idc=3 for ref pictures/param sets
	b = append(b, header)
	b = append(b, payload...)
	return b
}

func TestStartCodeLen(t *testing.T) {
	if l := startCodeLen([]byte{0, 0, 1, 0x67}, 0); l != 3 {
		t.Errorf("3-byte start code: got len %d, want 3", l)
	}
	if l := startCodeLen([]byte{0, 0, 0, 1, 0x67}, 0); l != 4 {
		t.Errorf("4-byte start code: got len %d, want 4", l)
	}
	if l := startCodeLen([]byte{0, 0, 2, 1}, 0); l != 0 {
		t.Errorf("non-start-code: got len %d, want 0", l)
	}
}

func TestSplitNALUsTypesAndOffsets(t *testing.T) {
	var buf []byte
	buf = append(buf, nal(4, nalTypeSPS, 0x42, 0x00)...)
	buf = append(buf, nal(3, nalTypePPS, 0xCE)...)
	buf = append(buf, nal(4, nalTypeIDR, 0x88, 0x99)...) // MSB=1 → first slice

	nalus := splitNALUs(buf)
	if len(nalus) != 3 {
		t.Fatalf("got %d NALUs, want 3", len(nalus))
	}
	wantTypes := []byte{nalTypeSPS, nalTypePPS, nalTypeIDR}
	for i, n := range nalus {
		if n.nalType != wantTypes[i] {
			t.Errorf("NALU %d type = %d, want %d", i, n.nalType, wantTypes[i])
		}
	}
	// IDR must be detected as first slice.
	if !nalus[2].firstSlice {
		t.Error("IDR NALU should have firstSlice=true (first_mb_in_slice==0)")
	}
	// Offsets must be contiguous and cover the whole buffer.
	if nalus[0].start != 0 {
		t.Errorf("first NALU start = %d, want 0", nalus[0].start)
	}
	if nalus[len(nalus)-1].end != len(buf) {
		t.Errorf("last NALU end = %d, want %d", nalus[len(nalus)-1].end, len(buf))
	}
	for i := 1; i < len(nalus); i++ {
		if nalus[i-1].end != nalus[i].start {
			t.Errorf("gap between NALU %d and %d: %d != %d", i-1, i, nalus[i-1].end, nalus[i].start)
		}
	}
}

func TestSplitNALUsNoStartCode(t *testing.T) {
	if got := splitNALUs([]byte{0x01, 0x02, 0x03}); got != nil {
		t.Errorf("expected nil for buffer without start code, got %v", got)
	}
}

// TestSplitterKeyframeAU verifies that an SPS+PPS+IDR sequence is folded into a
// single keyframe access unit, and that the following non-IDR slice starts a new
// non-keyframe AU.
func TestSplitterKeyframeAU(t *testing.T) {
	s := NewAnnexBSplitter()

	// Build: [SPS][PPS][IDR slice]  then  [non-IDR slice]  then  [non-IDR slice]
	// VCL payloads: MSB=1 (0x88, 0x91, 0xA2) → first_mb_in_slice==0 → each is
	// the sole slice of its picture.
	var stream []byte
	sps := nal(4, nalTypeSPS, 0x42)
	pps := nal(3, nalTypePPS, 0xCE)
	idr := nal(4, nalTypeIDR, 0x88)
	p1 := nal(4, nalTypeNonIDR, 0x91)
	p2 := nal(4, nalTypeNonIDR, 0xA2)

	stream = append(stream, sps...)
	stream = append(stream, pps...)
	stream = append(stream, idr...)
	stream = append(stream, p1...)
	stream = append(stream, p2...)

	aus := s.Push(stream)
	aus = append(aus, s.Flush()...)

	if len(aus) != 3 {
		t.Fatalf("got %d AUs, want 3", len(aus))
	}

	// AU 0: the keyframe, must contain SPS+PPS+IDR and be flagged keyframe.
	if !aus[0].isKeyframe {
		t.Error("AU 0 should be a keyframe (contains IDR)")
	}
	wantKey := append(append(append([]byte{}, sps...), pps...), idr...)
	if !bytes.Equal(aus[0].data, wantKey) {
		t.Errorf("keyframe AU data mismatch:\n got %x\nwant %x", aus[0].data, wantKey)
	}

	// AU 1 and AU 2: non-keyframe P-slices.
	if aus[1].isKeyframe || aus[2].isKeyframe {
		t.Error("P-frame AUs should not be keyframes")
	}
	if !bytes.Equal(aus[1].data, p1) {
		t.Errorf("AU 1 data mismatch:\n got %x\nwant %x", aus[1].data, p1)
	}
	if !bytes.Equal(aus[2].data, p2) {
		t.Errorf("AU 2 data mismatch:\n got %x\nwant %x", aus[2].data, p2)
	}
}

// TestSplitterChunked verifies that the splitter correctly reassembles access
// units when the byte stream is delivered in arbitrary chunks (as it would be
// when read in 32KB blocks from ffmpeg stdout).
func TestSplitterChunked(t *testing.T) {
	// All VCL payloads have MSB=1 → first_mb_in_slice==0 → each is a single-
	// slice picture (new AU).
	idr := nal(4, nalTypeIDR, 0x81, 0x02, 0x03, 0x04)
	p1 := nal(4, nalTypeNonIDR, 0x85, 0x06)
	p2 := nal(4, nalTypeNonIDR, 0x87, 0x08)
	full := append(append(append([]byte{}, idr...), p1...), p2...)

	// Feed one byte at a time and collect all AUs.
	s := NewAnnexBSplitter()
	var aus []accessUnit
	for i := 0; i < len(full); i++ {
		aus = append(aus, s.Push(full[i:i+1])...)
	}
	aus = append(aus, s.Flush()...)

	if len(aus) != 3 {
		t.Fatalf("chunked: got %d AUs, want 3", len(aus))
	}
	if !aus[0].isKeyframe {
		t.Error("chunked: AU 0 should be keyframe")
	}
	if !bytes.Equal(aus[0].data, idr) {
		t.Errorf("chunked AU0:\n got %x\nwant %x", aus[0].data, idr)
	}
	if !bytes.Equal(aus[1].data, p1) || !bytes.Equal(aus[2].data, p2) {
		t.Error("chunked: P-frame AU data mismatch")
	}
}

// TestSplitterAUDBoundary checks that an explicit access-unit delimiter forces a
// new AU even between two slices.
func TestSplitterAUDBoundary(t *testing.T) {
	s := NewAnnexBSplitter()
	var stream []byte
	stream = append(stream, nal(4, nalTypeIDR, 0xAA)...)    // MSB=1 → first slice
	stream = append(stream, nal(4, nalTypeAUD, 0x10)...)     // AUD
	stream = append(stream, nal(4, nalTypeNonIDR, 0xBB)...) // MSB=1 → first slice

	aus := append(s.Push(stream), s.Flush()...)
	if len(aus) != 2 {
		t.Fatalf("AUD boundary: got %d AUs, want 2", len(aus))
	}
	if !aus[0].isKeyframe {
		t.Error("AUD boundary: first AU should be keyframe")
	}
}

// TestSplitterMultiSlice verifies that a multi-slice frame (e.g. libx264 with
// sliced-threads) is grouped into a single access unit. This was the root cause
// of the green-screen / 1fps WebRTC bug: the old splitter treated every VCL
// NALU as a new AU boundary, sending each slice as a separate WebRTC sample.
func TestSplitterMultiSlice(t *testing.T) {
	s := NewAnnexBSplitter()

	// Frame 1 (IDR, 3 slices): SPS + PPS + IDR_slice0 + IDR_slice1 + IDR_slice2
	sps := nal(4, nalTypeSPS, 0x42)
	pps := nal(3, nalTypePPS, 0xCE)
	idr0 := nal(4, nalTypeIDR, 0x88, 0x01)       // MSB=1 → first_mb=0 (new picture)
	idr1 := nal(3, nalTypeIDR, 0x40, 0x02, 0x03)  // MSB=0 → first_mb>0 (continuation)
	idr2 := nal(3, nalTypeIDR, 0x60, 0x04)         // MSB=0 → first_mb>0 (continuation)

	// Frame 2 (P, 3 slices)
	p0 := nal(4, nalTypeNonIDR, 0x9E, 0x10)      // MSB=1 → first_mb=0 (new picture)
	p1 := nal(3, nalTypeNonIDR, 0x40, 0x20, 0x30) // MSB=0 → first_mb>0 (continuation)
	p2 := nal(3, nalTypeNonIDR, 0x60, 0x40)        // MSB=0 → first_mb>0 (continuation)

	// Frame 3 (P, 3 slices)
	q0 := nal(4, nalTypeNonIDR, 0xB0, 0x50)      // MSB=1 → first_mb=0 (new picture)
	q1 := nal(3, nalTypeNonIDR, 0x40, 0x60)       // MSB=0 → continuation
	q2 := nal(3, nalTypeNonIDR, 0x60, 0x70)       // MSB=0 → continuation

	var stream []byte
	stream = append(stream, sps...)
	stream = append(stream, pps...)
	stream = append(stream, idr0...)
	stream = append(stream, idr1...)
	stream = append(stream, idr2...)
	stream = append(stream, p0...)
	stream = append(stream, p1...)
	stream = append(stream, p2...)
	stream = append(stream, q0...)
	stream = append(stream, q1...)
	stream = append(stream, q2...)

	aus := s.Push(stream)
	aus = append(aus, s.Flush()...)

	if len(aus) != 3 {
		t.Fatalf("multi-slice: got %d AUs, want 3 (one per frame)", len(aus))
	}

	// AU 0: keyframe — must contain SPS + PPS + all 3 IDR slices.
	if !aus[0].isKeyframe {
		t.Error("AU 0 should be a keyframe")
	}
	var wantAU0 []byte
	wantAU0 = append(wantAU0, sps...)
	wantAU0 = append(wantAU0, pps...)
	wantAU0 = append(wantAU0, idr0...)
	wantAU0 = append(wantAU0, idr1...)
	wantAU0 = append(wantAU0, idr2...)
	if !bytes.Equal(aus[0].data, wantAU0) {
		t.Errorf("multi-slice AU0 data mismatch:\n got %x\nwant %x", aus[0].data, wantAU0)
	}

	// AU 1: P-frame — must contain all 3 P slices.
	if aus[1].isKeyframe {
		t.Error("AU 1 should not be a keyframe")
	}
	var wantAU1 []byte
	wantAU1 = append(wantAU1, p0...)
	wantAU1 = append(wantAU1, p1...)
	wantAU1 = append(wantAU1, p2...)
	if !bytes.Equal(aus[1].data, wantAU1) {
		t.Errorf("multi-slice AU1 data mismatch:\n got %x\nwant %x", aus[1].data, wantAU1)
	}

	// AU 2: P-frame — all 3 slices.
	var wantAU2 []byte
	wantAU2 = append(wantAU2, q0...)
	wantAU2 = append(wantAU2, q1...)
	wantAU2 = append(wantAU2, q2...)
	if !bytes.Equal(aus[2].data, wantAU2) {
		t.Errorf("multi-slice AU2 data mismatch:\n got %x\nwant %x", aus[2].data, wantAU2)
	}
}

// TestSplitterMultiSliceChunked verifies multi-slice grouping when the stream
// arrives byte-by-byte (worst-case chunking).
func TestSplitterMultiSliceChunked(t *testing.T) {
	// 2-slice IDR followed by 2-slice P
	idr0 := nal(4, nalTypeIDR, 0x88, 0xAA)
	idr1 := nal(3, nalTypeIDR, 0x40, 0xBB)
	p0 := nal(4, nalTypeNonIDR, 0x9C, 0xCC)
	p1 := nal(3, nalTypeNonIDR, 0x40, 0xDD)

	var full []byte
	full = append(full, idr0...)
	full = append(full, idr1...)
	full = append(full, p0...)
	full = append(full, p1...)

	s := NewAnnexBSplitter()
	var aus []accessUnit
	for i := 0; i < len(full); i++ {
		aus = append(aus, s.Push(full[i:i+1])...)
	}
	aus = append(aus, s.Flush()...)

	if len(aus) != 2 {
		t.Fatalf("multi-slice chunked: got %d AUs, want 2", len(aus))
	}

	wantIDR := append(append([]byte{}, idr0...), idr1...)
	if !bytes.Equal(aus[0].data, wantIDR) {
		t.Errorf("multi-slice chunked AU0:\n got %x\nwant %x", aus[0].data, wantIDR)
	}
	if !aus[0].isKeyframe {
		t.Error("multi-slice chunked AU0 should be keyframe")
	}

	wantP := append(append([]byte{}, p0...), p1...)
	if !bytes.Equal(aus[1].data, wantP) {
		t.Errorf("multi-slice chunked AU1:\n got %x\nwant %x", aus[1].data, wantP)
	}
}

func TestCloneBytes(t *testing.T) {
	src := []byte{1, 2, 3}
	dst := cloneBytes(src)
	if !bytes.Equal(src, dst) {
		t.Fatal("cloneBytes content mismatch")
	}
	dst[0] = 9
	if src[0] == 9 {
		t.Error("cloneBytes did not produce an independent copy")
	}
}
