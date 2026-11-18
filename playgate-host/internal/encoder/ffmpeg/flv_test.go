package ffmpeg

import (
	"bytes"
	"context"
	"encoding/binary"
	"os/exec"
	"testing"
	"time"

	"github.com/playgate/playgate-host/internal/core"
)

var (
	testSPS = []byte{0x67, 0x42, 0xe0, 0x2a}
	testPPS = []byte{0x68, 0xce, 0x3c, 0x80}
	testIDR = []byte{0x65, 0x88, 0x84, 0x00}
	testP   = []byte{0x41, 0x9a, 0x00, 0x01} // contains a start-code-like tail
	testSEI = []byte{0x06, 0x05, 0x01}
)

func flvFileHeader() []byte {
	return []byte{'F', 'L', 'V', 1, 0x01, 0, 0, 0, 9, 0, 0, 0, 0}
}

func flvTag(tagType byte, data []byte) []byte {
	out := []byte{tagType, byte(len(data) >> 16), byte(len(data) >> 8), byte(len(data)), 0, 0, 0, 0, 0, 0, 0}
	out = append(out, data...)
	return binary.BigEndian.AppendUint32(out, uint32(11+len(data)))
}

func flvVideo(keyframe bool, avcPacketType byte, body []byte) []byte {
	frameType := byte(2)
	if keyframe {
		frameType = 1
	}
	return flvTag(flvTagVideo, append([]byte{frameType<<4 | flvCodecAVC, avcPacketType, 0, 0, 0}, body...))
}

func avcConfig(sps, pps []byte) []byte {
	rec := []byte{1, sps[1], sps[2], sps[3], 0xff, 0xe1}
	rec = binary.BigEndian.AppendUint16(rec, uint16(len(sps)))
	rec = append(rec, sps...)
	rec = append(rec, 1)
	rec = binary.BigEndian.AppendUint16(rec, uint16(len(pps)))
	return append(rec, pps...)
}

func avcc(nalus ...[]byte) []byte {
	var out []byte
	for _, n := range nalus {
		out = binary.BigEndian.AppendUint32(out, uint32(len(n)))
		out = append(out, n...)
	}
	return out
}

func annexB(nalus ...[]byte) []byte {
	var out []byte
	for _, n := range nalus {
		out = append(out, 0, 0, 0, 1)
		out = append(out, n...)
	}
	return out
}

func testStream() []byte {
	s := flvFileHeader()
	s = append(s, flvTag(18, []byte("onMetaData"))...) // script tag: ignored
	s = append(s, flvVideo(true, flvAVCSeqHeader, avcConfig(testSPS, testPPS))...)
	s = append(s, flvVideo(true, flvAVCNALU, avcc(testSEI, testIDR))...)
	s = append(s, flvVideo(false, flvAVCNALU, avcc(testP))...)
	return s
}

func checkTestStream(t *testing.T, aus []accessUnit) {
	t.Helper()
	if len(aus) != 2 {
		t.Fatalf("got %d access units, want 2", len(aus))
	}
	if !aus[0].isKeyframe || aus[1].isKeyframe {
		t.Errorf("keyframe flags = %v,%v; want true,false", aus[0].isKeyframe, aus[1].isKeyframe)
	}
	// The IDR carries the configuration record's SPS/PPS so it decodes standalone.
	if want := annexB(testSPS, testPPS, testSEI, testIDR); !bytes.Equal(aus[0].data, want) {
		t.Errorf("IDR AU = % x\nwant     % x", aus[0].data, want)
	}
	if want := annexB(testP); !bytes.Equal(aus[1].data, want) {
		t.Errorf("P AU = % x, want % x", aus[1].data, want)
	}
}

func TestFLVDemuxerWholeStream(t *testing.T) {
	aus, err := NewFLVDemuxer().Push(testStream())
	if err != nil {
		t.Fatal(err)
	}
	checkTestStream(t, aus)
}

// TestFLVDemuxerEmitsOnTagCompletion is the point of using FLV: a picture is
// returned by the Push that completes its tag, without waiting for the next one.
func TestFLVDemuxerEmitsOnTagCompletion(t *testing.T) {
	d := NewFLVDemuxer()
	head := append(flvFileHeader(), flvVideo(true, flvAVCSeqHeader, avcConfig(testSPS, testPPS))...)
	if aus, err := d.Push(head); err != nil || len(aus) != 0 {
		t.Fatalf("header: aus=%d err=%v", len(aus), err)
	}
	tag := flvVideo(true, flvAVCNALU, avcc(testIDR))
	if aus, err := d.Push(tag[:len(tag)-1]); err != nil || len(aus) != 0 {
		t.Fatalf("incomplete tag: aus=%d err=%v", len(aus), err)
	}
	aus, err := d.Push(tag[len(tag)-1:])
	if err != nil || len(aus) != 1 {
		t.Fatalf("completed tag: aus=%d err=%v", len(aus), err)
	}
}

func TestFLVDemuxerByteAtATime(t *testing.T) {
	d := NewFLVDemuxer()
	var aus []accessUnit
	for _, b := range testStream() {
		got, err := d.Push([]byte{b})
		if err != nil {
			t.Fatal(err)
		}
		aus = append(aus, got...)
	}
	checkTestStream(t, aus)
}

// TestFLVDemuxerAnnexBPassthrough covers encoders that give ffmpeg no global
// header: no configuration record is written and tag bodies are Annex-B.
func TestFLVDemuxerAnnexBPassthrough(t *testing.T) {
	s := flvFileHeader()
	s = append(s, flvVideo(true, flvAVCNALU, annexB(testSPS, testPPS, testIDR))...)
	s = append(s, flvVideo(false, flvAVCNALU, annexB(testP))...)
	aus, err := NewFLVDemuxer().Push(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(aus) != 2 || !aus[0].isKeyframe || aus[1].isKeyframe {
		t.Fatalf("aus=%d", len(aus))
	}
	if want := annexB(testSPS, testPPS, testIDR); !bytes.Equal(aus[0].data, want) {
		t.Errorf("IDR AU = % x, want % x", aus[0].data, want)
	}
}

func TestFLVDemuxerRejectsGarbage(t *testing.T) {
	if _, err := NewFLVDemuxer().Push(annexB(testSPS, testPPS, testIDR, testP)); err == nil {
		t.Error("expected an error for a non-FLV stream")
	}
	d := NewFLVDemuxer()
	s := append(flvFileHeader(), flvVideo(true, flvAVCSeqHeader, avcConfig(testSPS, testPPS))...)
	s = append(s, flvVideo(true, flvAVCNALU, []byte{0, 0, 1, 0, 0x65})...) // NAL length 256, 1 byte present
	if _, err := d.Push(s); err == nil {
		t.Error("expected an error for a NAL unit overrunning its tag")
	}
}

// TestEncoderEmitsEveryFrameWithoutLookahead runs the real ffmpeg (skipped when
// absent): after N frames go in and input pauses, N packets must come out. With
// a raw Annex-B output the last one would be held until frame N+1 arrived.
func TestEncoderEmitsEveryFrameWithoutLookahead(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	const (
		w, h, n  = 320, 240, 8
		interval = 40 * time.Millisecond
	)
	in := make(chan core.VideoFrame)
	opts := DefaultOptions(w, h, 30, core.PixelFormatYUYV)
	enc, err := New(quietLogger(), opts, in)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = enc.Run(ctx) }()

	base := time.Now()
	go func() {
		for i := 0; i < n; i++ {
			data := bytes.Repeat([]byte{byte(16 + i*20), 128}, w*h)
			select {
			case in <- core.VideoFrame{Data: data, PixelFormat: core.PixelFormatYUYV, Width: w, Height: h,
				Timestamp: base.Add(time.Duration(i) * interval)}:
			case <-ctx.Done():
				return
			}
			time.Sleep(interval)
		}
	}()

	var pkts []core.EncodedPacket
	deadline := time.After(10 * time.Second)
	for len(pkts) < n {
		select {
		case p, ok := <-enc.Packets():
			if !ok {
				t.Fatalf("encoder stopped after %d packets", len(pkts))
			}
			pkts = append(pkts, p)
		case <-deadline:
			t.Fatalf("got %d packets for %d frames; the last frame is being held back", len(pkts), n)
		}
	}

	if !pkts[0].IsKeyframe {
		t.Error("first packet should be a keyframe")
	}
	if !bytes.HasPrefix(pkts[0].Data, []byte{0, 0, 0, 1, 0x67}) {
		t.Errorf("keyframe should start with an Annex-B SPS, got % x", pkts[0].Data[:8])
	}
	for i, p := range pkts {
		if want := time.Duration(i) * interval; p.PTS != want {
			t.Errorf("packet %d PTS = %v, want capture-time offset %v", i, p.PTS, want)
		}
		if p.EncodeStart.IsZero() {
			t.Errorf("packet %d has no EncodeStart", i)
		}
	}
	cancel()
	<-done
}
