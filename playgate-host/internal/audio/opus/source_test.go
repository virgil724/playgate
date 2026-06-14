package opus

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"time"
)

// buildOggPage assembles one Ogg page (RFC 3533) with the given lacing table and
// data so tests can exercise the demuxer without invoking ffmpeg. The CRC is left
// zero — the demuxer does not verify it.
func buildOggPage(headerType byte, granule uint64, seq uint32, segTable, data []byte) []byte {
	var b bytes.Buffer
	b.WriteString("OggS")
	b.WriteByte(0) // version
	b.WriteByte(headerType)
	var tmp [8]byte
	binary.LittleEndian.PutUint64(tmp[:], granule)
	b.Write(tmp[:])
	binary.LittleEndian.PutUint32(tmp[:4], 1) // serial
	b.Write(tmp[:4])
	binary.LittleEndian.PutUint32(tmp[:4], seq) // page sequence
	b.Write(tmp[:4])
	b.Write([]byte{0, 0, 0, 0}) // crc (ignored)
	b.WriteByte(byte(len(segTable)))
	b.Write(segTable)
	b.Write(data)
	return b.Bytes()
}

// toc20ms is an Opus TOC byte for a 20 ms, single-frame packet (config 15 =
// fullband hybrid 20 ms, code 0).
const toc20ms = byte(15 << 3)

func TestReadPagesSplitsPacketsAndSpansPages(t *testing.T) {
	// Header page: one OpusHead packet, must be skipped.
	head := append([]byte("OpusHead"), make([]byte, 11)...) // 19-byte head
	headPage := buildOggPage(0x02, 0, 0, []byte{byte(len(head))}, head)

	// Data page with TWO Opus packets in one page (the case that broke playback
	// when written as a single RTP sample).
	pkt1 := []byte{toc20ms, 0xaa, 0xbb}       // len 3
	pkt2 := []byte{toc20ms, 0x01, 0x02, 0x03} // len 4
	dataPage := buildOggPage(0x00, 960, 1,
		[]byte{byte(len(pkt1)), byte(len(pkt2))},
		append(append([]byte{}, pkt1...), pkt2...))

	// A packet that spans two pages: 255-byte segment continues, finished by a
	// 10-byte segment on the next (continued) page.
	spanHead := make([]byte, 255)
	spanHead[0] = toc20ms
	spanTail := make([]byte, 10)
	pageB := buildOggPage(0x00, 1920, 2, []byte{255}, spanHead)
	pageC := buildOggPage(0x01, 2880, 3, []byte{10}, spanTail)

	stream := bytes.Join([][]byte{headPage, dataPage, pageB, pageC}, nil)

	s := New(nil, DefaultConfig())
	go func() {
		_ = s.readPages(context.Background(), bytes.NewReader(stream))
	}()

	var got []Packet
	timeout := time.After(2 * time.Second)
	for len(got) < 3 {
		select {
		case p := <-s.Packets():
			got = append(got, p)
		case <-timeout:
			t.Fatalf("only received %d packets, want 3", len(got))
		}
	}

	wantLens := []int{3, 4, 265}
	for i, w := range wantLens {
		if len(got[i].Data) != w {
			t.Errorf("packet %d len = %d, want %d", i, len(got[i].Data), w)
		}
		if got[i].Duration != 20*time.Millisecond {
			t.Errorf("packet %d duration = %v, want 20ms", i, got[i].Duration)
		}
	}
}

func TestOpusPacketDuration(t *testing.T) {
	cases := []struct {
		name string
		toc  byte
		b1   byte // second byte, used only for code 3
		want time.Duration
	}{
		{"silk 20ms config1 code0", 1 << 3, 0, 20 * time.Millisecond},
		{"silk 60ms config3 code0", 3 << 3, 0, 60 * time.Millisecond},
		{"hybrid 10ms config14 code0", 14 << 3, 0, 10 * time.Millisecond},
		{"celt 20ms config19 code0", 19 << 3, 0, 20 * time.Millisecond},
		{"celt 2.5ms config16 code0", 16 << 3, 0, 2500 * time.Microsecond},
		{"code1 two frames", (15 << 3) | 1, 0, 40 * time.Millisecond},
		{"code3 three frames", (15 << 3) | 3, 3, 60 * time.Millisecond},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := opusPacketDuration([]byte{c.toc, c.b1}); got != c.want {
				t.Errorf("opusPacketDuration = %v, want %v", got, c.want)
			}
		})
	}
}
