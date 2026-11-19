//go:build cgo && libav

package libav

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand"
	"testing"
	"time"

	"github.com/playgate/playgate-host/internal/core"
	"github.com/playgate/playgate-host/internal/encoder/ffmpeg"
)

const (
	tw, th, tfps = 320, 240, 30
)

func testOpts(bitrate, gop int) ffmpeg.Options {
	o := ffmpeg.DefaultOptions(tw, th, tfps, core.PixelFormatNV12)
	o.Bitrate = bitrate
	o.GOPSize = gop
	return o
}

// synthFrame returns a moving, noisy NV12 frame so the encoder has real work to
// do and bitrate changes show up in the output size.
func synthFrame(n int, rng *rand.Rand) core.VideoFrame {
	data := make([]byte, tw*th*3/2)
	for y := 0; y < th; y++ {
		for x := 0; x < tw; x++ {
			data[y*tw+x] = byte((x+n*3)^(y+n*2)) + byte(rng.Intn(24))
		}
	}
	for i := tw * th; i < len(data); i++ {
		data[i] = byte(128 + rng.Intn(16))
	}
	return core.VideoFrame{
		Data: data, PixelFormat: core.PixelFormatNV12, Width: tw, Height: th,
		Timestamp: time.Unix(0, 0).Add(time.Duration(n) * time.Second / tfps),
	}
}

// nalTypes lists the NAL unit types in an Annex-B access unit.
func nalTypes(b []byte) []int {
	var out []int
	for i := 0; i+3 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			out = append(out, int(b[i+3]&0x1f))
			i += 3
		}
	}
	return out
}

func has(ts []int, t int) bool {
	for _, x := range ts {
		if x == t {
			return true
		}
	}
	return false
}

// harness feeds frames one at a time (waiting for each packet) so keyframe and
// bitrate requests are applied at a deterministic frame boundary.
type harness struct {
	t   *testing.T
	enc core.VideoEncoder
	in  chan core.VideoFrame
	rng *rand.Rand
	n   int
	end func()
}

func newHarness(t *testing.T, o ffmpeg.Options) *harness {
	t.Helper()
	in := make(chan core.VideoFrame)
	enc, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)), o, in)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = enc.Run(ctx) }()
	h := &harness{t: t, enc: enc, in: in, rng: rand.New(rand.NewSource(1))}
	h.end = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return")
		}
		for range enc.Packets() { // closed by Run on exit
		}
	}
	return h
}

func (h *harness) next() core.EncodedPacket {
	h.t.Helper()
	h.in <- synthFrame(h.n, h.rng)
	h.n++
	select {
	case p, ok := <-h.enc.Packets():
		if !ok {
			h.t.Fatal("packets closed")
		}
		return p
	case <-time.After(5 * time.Second):
		h.t.Fatal("timeout waiting for packet")
	}
	return core.EncodedPacket{}
}

func TestEncodeNV12(t *testing.T) {
	h := newHarness(t, testOpts(1_000_000, 30))
	defer h.end()
	var lastPTS time.Duration = -1
	for i := 0; i < 20; i++ {
		p := h.next()
		ts := nalTypes(p.Data)
		if i == 0 {
			if !p.IsKeyframe || !has(ts, 5) || !has(ts, 7) || !has(ts, 8) {
				t.Fatalf("first AU: key=%v nals=%v, want in-band SPS/PPS + IDR", p.IsKeyframe, ts)
			}
		}
		if p.PTS <= lastPTS {
			t.Fatalf("PTS not increasing: %v after %v", p.PTS, lastPTS)
		}
		if p.EncodeStart.IsZero() {
			t.Fatal("EncodeStart not set")
		}
		lastPTS = p.PTS
	}
	if want := 19 * time.Second / tfps; lastPTS != want {
		t.Fatalf("PTS from capture timestamps: got %v want %v", lastPTS, want)
	}
}

func TestForceKeyframeYieldsIDR(t *testing.T) {
	h := newHarness(t, testOpts(1_000_000, 300)) // long GOP: no scheduled IDR
	defer h.end()
	for i := 0; i < 10; i++ {
		p := h.next()
		if i > 0 && (p.IsKeyframe || has(nalTypes(p.Data), 5)) {
			t.Fatalf("frame %d: unexpected IDR before ForceKeyframe", i)
		}
	}
	h.enc.ForceKeyframe()
	p := h.next()
	ts := nalTypes(p.Data)
	if !p.IsKeyframe || !has(ts, 5) {
		t.Fatalf("after ForceKeyframe: key=%v nals=%v, want IDR (NAL 5)", p.IsKeyframe, ts)
	}
	if !has(ts, 7) || !has(ts, 8) {
		t.Fatalf("forced IDR lacks in-band SPS/PPS: nals=%v", ts)
	}
	if p2 := h.next(); p2.IsKeyframe {
		t.Fatalf("second frame after forced IDR is a keyframe, request should be one-shot")
	}
}

func TestSetBitrateReducesSize(t *testing.T) {
	h := newHarness(t, testOpts(2_000_000, 300))
	defer h.end()
	avg := func(n int) float64 {
		total := 0
		for i := 0; i < n; i++ {
			total += len(h.next().Data)
		}
		return float64(total) / float64(n)
	}
	avg(30) // warm up rate control
	before := avg(60)
	h.enc.SetBitrate(300_000)
	avg(30) // let VBV settle
	after := avg(60)
	t.Logf("avg AU bytes: before=%.0f after=%.0f", before, after)
	if after > before*0.5 {
		t.Fatalf("SetBitrate(300k) did not materially reduce AU size: before=%.0f after=%.0f", before, after)
	}
}

func TestIntraRefresh(t *testing.T) {
	o := testOpts(1_000_000, 10) // GOP 10: periodic IDRs would show within 40 frames
	o.IntraRefresh = true
	h := newHarness(t, o)
	defer h.end()
	for i := 0; i < 40; i++ {
		p := h.next()
		ts := nalTypes(p.Data)
		if i == 0 {
			if !p.IsKeyframe || !has(ts, 5) || !has(ts, 7) || !has(ts, 8) {
				t.Fatalf("first AU: key=%v nals=%v, want in-band SPS/PPS + IDR", p.IsKeyframe, ts)
			}
			continue
		}
		if has(ts, 5) {
			t.Fatalf("frame %d: IDR (NAL 5) with intra refresh enabled, nals=%v", i, ts)
		}
	}

	// A consumer that stops reading drops packets; with intra refresh no IDR will
	// ever come, so the emitter must not wait for one and must resume on its own.
	for i := 0; i < 30; i++ {
		h.in <- synthFrame(h.n, h.rng)
		h.n++
	}
	time.Sleep(500 * time.Millisecond) // let the encoder finish its queue
	for drained := false; !drained; {
		select {
		case <-h.enc.Packets():
		default:
			drained = true
		}
	}
	for i := 0; i < 10; i++ {
		p := h.next()
		if has(nalTypes(p.Data), 5) {
			t.Fatalf("frame after drop %d: unexpected IDR", i)
		}
	}
}

func TestCaps(t *testing.T) {
	enc, err := New(nil, testOpts(1_000_000, 30), make(chan core.VideoFrame))
	if err != nil {
		t.Fatal(err)
	}
	if c := enc.Caps(); !c.LiveBitrate || !c.CheapKeyframe {
		t.Fatalf("caps = %+v", c)
	}
	// Never Run: release the codec through a cancelled Run so resources free.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = enc.Run(ctx)
}

func TestUnsupported(t *testing.T) {
	o := testOpts(1_000_000, 30)
	o.InputFormat = core.PixelFormatMJPEG
	if _, err := New(nil, o, nil); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("MJPEG: err = %v, want ErrUnsupported", err)
	}
	o = testOpts(1_000_000, 30)
	o.Codec = ffmpeg.VAAPI{}
	if _, err := New(nil, o, nil); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("vaapi: err = %v, want ErrUnsupported", err)
	}
}

func TestYUYVInput(t *testing.T) {
	o := testOpts(1_000_000, 30)
	o.InputFormat = core.PixelFormatYUYV
	in := make(chan core.VideoFrame, 1)
	enc, err := New(nil, o, in)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = enc.Run(ctx) }()
	data := make([]byte, tw*th*2)
	for i := range data {
		data[i] = byte(i)
	}
	in <- core.VideoFrame{Data: data, PixelFormat: core.PixelFormatYUYV, Width: tw, Height: th}
	select {
	case p := <-enc.Packets():
		if !p.IsKeyframe {
			t.Fatal("first YUYV AU not a keyframe")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
}
