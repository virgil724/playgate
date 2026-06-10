package ffmpeg

import (
	"bytes"
	"testing"
)

// nal builds one NAL unit: a start code (3 or 4 bytes) followed by a payload
// whose first byte encodes the given NAL type in its low 5 bits.
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
	buf = append(buf, nal(4, nalTypeIDR, 0x88, 0x99)...)

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
	var stream []byte
	sps := nal(4, nalTypeSPS, 0x42)
	pps := nal(3, nalTypePPS, 0xCE)
	idr := nal(4, nalTypeIDR, 0x88)
	p1 := nal(4, nalTypeNonIDR, 0x11)
	p2 := nal(4, nalTypeNonIDR, 0x22)

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
	idr := nal(4, nalTypeIDR, 0x01, 0x02, 0x03, 0x04)
	p1 := nal(4, nalTypeNonIDR, 0x05, 0x06)
	p2 := nal(4, nalTypeNonIDR, 0x07, 0x08)
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
	stream = append(stream, nal(4, nalTypeIDR, 0xAA)...)
	stream = append(stream, nal(4, nalTypeAUD, 0x10)...)
	stream = append(stream, nal(4, nalTypeNonIDR, 0xBB)...)

	aus := append(s.Push(stream), s.Flush()...)
	if len(aus) != 2 {
		t.Fatalf("AUD boundary: got %d AUs, want 2", len(aus))
	}
	if !aus[0].isKeyframe {
		t.Error("AUD boundary: first AU should be keyframe")
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
