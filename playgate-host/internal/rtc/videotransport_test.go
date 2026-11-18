package rtc

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/playgate/playgate-host/internal/core"
)

// goldenHeader is the vector from docs/webcodecs-transport.md section 9.
var goldenHeader = []byte{
	0x01, 0x03, 0x00, 0x01, 0x00, 0x00, 0x00, 0x10, 0x00, 0x01, 0x00, 0x00,
	0x00, 0x00, 0x09, 0x60, 0x00, 0x01, 0x00, 0x03, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x64,
}

func TestVideoHeaderGolden(t *testing.T) {
	h := VideoHeader{
		Version: 1, Flags: FlagKey | FlagParam, Epoch: 1, FrameID: 0x10, PTSUs: 0x10000,
		FrameSize: 2400, FragIdx: 1, DataCount: 3, ParityCount: 0, Codec: 0, FECScheme: 0, SendMS: 0x64,
	}
	got := make([]byte, VideoHeaderSize)
	h.Encode(got)
	if !bytes.Equal(got, goldenHeader) {
		t.Fatalf("encode mismatch\n got %x\nwant %x", got, goldenHeader)
	}
	back, payload, err := ParseVideoHeader(append(append([]byte{}, goldenHeader...), 0xAA))
	if err != nil {
		t.Fatal(err)
	}
	if back != h || len(payload) != 1 {
		t.Fatalf("decode mismatch: %+v payload=%d", back, len(payload))
	}
	if FragmentCount(2400, 1172) != 3 {
		t.Fatal("2400 bytes at stride 1172 must be k=3")
	}
	if _, _, err := ParseVideoHeader(goldenHeader[:27]); err == nil {
		t.Fatal("short header must error")
	}
	bad := append([]byte{}, goldenHeader...)
	bad[0] = 2
	if _, _, err := ParseVideoHeader(bad); err == nil {
		t.Fatal("unknown version must error")
	}
}

// testAU builds an Annex-B access unit of about size bytes with optional SPS+PPS.
// The filler never forms a start code.
func testAU(size int, key bool) []byte {
	var out []byte
	if key {
		sc := []byte{0, 0, 0, 1}
		out = append(out, sc...)
		out = append(out, 0x67, 0x42, 0xe0, 0x2a, 0xf8)
		out = append(out, sc...)
		out = append(out, 0x68, 0xce, 0x38, 0x80)
		out = append(out, sc...)
		out = append(out, 0x65)
	} else {
		out = append(out, 0, 0, 0, 1, 0x41)
	}
	for len(out) < size {
		out = append(out, 0xAB)
	}
	return out
}

// reassemble is a minimal receiver used to validate the sender.
func reassemble(t *testing.T, msgs [][]byte) (VideoHeader, []byte) {
	t.Helper()
	var first VideoHeader
	var buf []byte
	got := map[uint16]bool{}
	stride := 0
	for i, m := range msgs {
		h, payload, err := ParseVideoHeader(m)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = h
			buf = make([]byte, h.FrameSize)
		}
		if h.FragIdx == 0 {
			stride = len(payload)
		}
		if h.FrameID != first.FrameID || h.FrameSize != first.FrameSize || h.DataCount != first.DataCount {
			t.Fatalf("inconsistent fragment header: %+v vs %+v", h, first)
		}
		if int(h.FragIdx) < int(h.DataCount)-1 && len(payload) != stride {
			t.Fatalf("non-last fragment %d has length %d, want %d", h.FragIdx, len(payload), stride)
		}
		copy(buf[int(h.FragIdx)*stride:], payload)
		got[h.FragIdx] = true
	}
	if len(got) != int(first.DataCount) {
		t.Fatalf("got %d distinct fragments, want %d", len(got), first.DataCount)
	}
	return first, buf
}

func TestFragmentRoundTrip(t *testing.T) {
	const stride = DefaultVideoFragmentBytes - VideoHeaderSize
	for _, size := range []int{1, 100, stride - 1, stride, stride + 1, 2400, 10 * stride, 10*stride + 7} {
		au := testAU(size, false)
		if len(au) < size {
			au = append(au, make([]byte, size-len(au))...)
		}
		msgs, err := FragmentFrame(FrameMeta{Flags: 0, Epoch: 3, FrameID: 9, PTSUs: 123, SendMS: 5}, au, stride)
		if err != nil {
			t.Fatal(err)
		}
		if want := FragmentCount(len(au), stride); len(msgs) != want {
			t.Fatalf("size %d: %d fragments, want %d", len(au), len(msgs), want)
		}
		for _, m := range msgs {
			if len(m) > DefaultVideoFragmentBytes {
				t.Fatalf("message %d exceeds %d", len(m), DefaultVideoFragmentBytes)
			}
		}
		h, out := reassemble(t, msgs)
		if !bytes.Equal(out, au) {
			t.Fatalf("size %d: reassembled bytes differ", len(au))
		}
		if h.Epoch != 3 || h.FrameID != 9 || h.PTSUs != 123 || h.SendMS != 5 || h.Codec != CodecH264 {
			t.Fatalf("header fields wrong: %+v", h)
		}
	}
	if _, err := FragmentFrame(FrameMeta{}, nil, stride); err == nil {
		t.Fatal("empty frame must error")
	}
}

func TestParamSets(t *testing.T) {
	ps, sps, pps := paramSets(testAU(100, true))
	if !sps || !pps || len(ps) != 5+4+4+4 {
		t.Fatalf("sps=%v pps=%v len=%d", sps, pps, len(ps))
	}
	if _, sps, pps := paramSets(testAU(100, false)); sps || pps {
		t.Fatal("delta frame must have no parameter sets")
	}
	// 3-byte start codes too.
	_, sps, pps = paramSets([]byte{0, 0, 1, 0x67, 1, 2, 0, 0, 1, 0x68, 3, 0, 0, 1, 0x65, 9})
	if !sps || !pps {
		t.Fatal("3-byte start codes not recognised")
	}
}

func TestFeedbackParsing(t *testing.T) {
	kr := make([]byte, KeyframeRequestSize)
	kr[0], kr[1] = 1, FBKeyframeRequest
	binary.BigEndian.PutUint16(kr[4:], 7)
	kr[6] = ReasonFrameLost
	binary.BigEndian.PutUint32(kr[8:], 0xFFFFFFFF)
	binary.BigEndian.PutUint32(kr[12:], 42)
	v, err := ParseFeedback(kr)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.(KeyframeRequest); got != (KeyframeRequest{Epoch: 7, Reason: ReasonFrameLost, LastGoodID: 0xFFFFFFFF, LostID: 42}) {
		t.Fatalf("KEYFRAME_REQUEST = %+v", got)
	}

	fr := make([]byte, FrameReportSize)
	fr[0], fr[1] = 1, FBFrameReport
	binary.BigEndian.PutUint32(fr[4:], 1000)
	for i := 0; i < 8; i++ {
		binary.BigEndian.PutUint16(fr[8+2*i:], uint16(i+1))
	}
	v, err = ParseFeedback(fr)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.(FrameReport); got != (FrameReport{HighestID: 1000, FramesComplete: 1, FramesLost: 2, FragsRecv: 3,
		FragsExpected: 4, Recovered: 5, DecodeQueue: 6, JitterMSx10: 7, DecMSx10: 8}) {
		t.Fatalf("FRAME_REPORT = %+v", got)
	}

	hello := make([]byte, HelloSize)
	hello[0], hello[1], hello[4], hello[5] = 1, FBHello, CapWebCodecsH264|CapAnnexB, 2
	binary.BigEndian.PutUint16(hello[6:], 250)
	v, err = ParseFeedback(hello)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.(Hello); got != (Hello{Caps: CapWebCodecsH264 | CapAnnexB, MaxFEC: 2, ReportMS: 250}) {
		t.Fatalf("HELLO = %+v", got)
	}

	// Unknown version/type are ignored without error; truncated known types error.
	for _, b := range [][]byte{{2, 1, 0, 0}, {1, 0x7f, 0, 0}, {1}, nil} {
		if v, err := ParseFeedback(b); v != nil || err != nil {
			t.Fatalf("%x: want (nil,nil), got (%v,%v)", b, v, err)
		}
	}
	for _, b := range [][]byte{kr[:15], fr[:23], hello[:11]} {
		if _, err := ParseFeedback(b); err == nil {
			t.Fatalf("truncated %x must error", b)
		}
	}
}

// --- sender policy tests with a fake channel ---

type fakeDC struct {
	mu       sync.Mutex
	msgs     [][]byte
	buffered uint64
	state    webrtc.DataChannelState
}

func (f *fakeDC) Send(b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, append([]byte(nil), b...))
	return nil
}
func (f *fakeDC) BufferedAmount() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buffered
}
func (f *fakeDC) ReadyState() webrtc.DataChannelState { return f.state }
func (f *fakeDC) setBuffered(n uint64) {
	f.mu.Lock()
	f.buffered = n
	f.mu.Unlock()
}
func (f *fakeDC) take() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.msgs
	f.msgs = nil
	return m
}

func helloMsg(caps uint8) []byte {
	b := make([]byte, HelloSize)
	b[0], b[1], b[4] = 1, FBHello, caps
	return b
}

func keyReq(epoch uint16) []byte {
	b := make([]byte, KeyframeRequestSize)
	b[0], b[1] = 1, FBKeyframeRequest
	binary.BigEndian.PutUint16(b[4:], epoch)
	return b
}

func newTestSender(mode string, dc dcSender) (*videoSender, *atomic.Int32, *[][]byte) {
	var wants atomic.Int32
	var acks [][]byte
	s := newVideoSender(videoSenderConfig{
		Mode: mode, BufferMax: 1000, IDRPaceMS: -1,
		WantKeyframe: func() { wants.Add(1) },
		SendAck:      func(b []byte) { acks = append(acks, b) },
	})
	s.setChannel(dc)
	return s, &wants, &acks
}

func pkt(au []byte, pts time.Duration, key bool) core.EncodedPacket {
	return core.EncodedPacket{Data: au, PTS: pts, IsKeyframe: key}
}

func TestSenderHelloGatingAndIDROnly(t *testing.T) {
	dc := &fakeDC{state: webrtc.DataChannelStateOpen}
	s, wants, _ := newTestSender(VideoTransportWebCodecs, dc)

	// Nothing before HELLO, even an IDR.
	_ = s.writeFrame(pkt(testAU(3000, true), 0, true))
	if len(dc.take()) != 0 {
		t.Fatal("video sent before HELLO")
	}
	s.handleFeedback(helloMsg(CapWebCodecsH264))
	if wants.Load() != 1 {
		t.Fatalf("HELLO must request a keyframe, wants=%d", wants.Load())
	}
	// Delta before the first IDR is dropped.
	_ = s.writeFrame(pkt(testAU(500, false), 10*time.Millisecond, false))
	if len(dc.take()) != 0 {
		t.Fatal("delta sent before first IDR")
	}
	_ = s.writeFrame(pkt(testAU(3000, true), 20*time.Millisecond, true))
	idr := dc.take()
	h, _ := reassemble(t, idr)
	if h.Flags != FlagKey|FlagParam || h.FrameID != 1 || h.Epoch != 0 {
		t.Fatalf("idr header %+v", h)
	}
	_ = s.writeFrame(pkt(testAU(500, false), 30*time.Millisecond, false))
	d := dc.take()
	h, _ = reassemble(t, d)
	if h.Flags != 0 || h.FrameID != 2 || h.PTSUs != 30000 {
		t.Fatalf("delta header %+v", h)
	}
}

func TestSenderDropPolicy(t *testing.T) {
	dc := &fakeDC{state: webrtc.DataChannelStateOpen}
	s, wants, _ := newTestSender(VideoTransportWebCodecs, dc)
	s.handleFeedback(helloMsg(CapWebCodecsH264))
	_ = s.writeFrame(pkt(testAU(2000, true), 0, true))
	dc.take()
	wants.Store(0)

	// Congested: whole frame dropped (no partial), keyframe requested.
	dc.setBuffered(5000)
	_ = s.writeFrame(pkt(testAU(3000, false), 10*time.Millisecond, false))
	if len(dc.take()) != 0 {
		t.Fatal("frame (or part of one) sent while congested")
	}
	if wants.Load() != 1 {
		t.Fatalf("drop must request keyframe, wants=%d", wants.Load())
	}
	// Congestion cleared, but deltas stay dropped until an IDR.
	dc.setBuffered(0)
	_ = s.writeFrame(pkt(testAU(800, false), 20*time.Millisecond, false))
	if len(dc.take()) != 0 {
		t.Fatal("delta sent after drop before next IDR")
	}
	// A dropped IDR keeps the gate shut.
	dc.setBuffered(5000)
	_ = s.writeFrame(pkt(testAU(3000, true), 30*time.Millisecond, true))
	dc.setBuffered(0)
	_ = s.writeFrame(pkt(testAU(800, false), 40*time.Millisecond, false))
	if len(dc.take()) != 0 {
		t.Fatal("delta sent after dropped IDR")
	}
	_ = s.writeFrame(pkt(testAU(3000, true), 50*time.Millisecond, true))
	if len(dc.take()) == 0 {
		t.Fatal("IDR not sent after congestion cleared")
	}
	_ = s.writeFrame(pkt(testAU(800, false), 60*time.Millisecond, false))
	if len(dc.take()) == 0 {
		t.Fatal("delta not resumed after IDR")
	}
	if _, dropped := s.stats(); dropped != 2 {
		t.Fatalf("dropped = %d, want 2", dropped)
	}
}

func TestSenderKeyframeRequestEpochRule(t *testing.T) {
	dc := &fakeDC{state: webrtc.DataChannelStateOpen}
	s, wants, _ := newTestSender(VideoTransportWebCodecs, dc)
	s.handleFeedback(helloMsg(CapWebCodecsH264))
	_ = s.writeFrame(pkt(testAU(2000, true), 0, true))
	dc.take()
	wants.Store(0)

	s.handleFeedback(keyReq(0))
	if wants.Load() != 1 {
		t.Fatal("current-epoch request must reach the encoder")
	}
	_ = s.writeFrame(pkt(testAU(500, false), 10*time.Millisecond, false))
	if len(dc.take()) != 0 {
		t.Fatal("deltas must stop after KEYFRAME_REQUEST until an IDR")
	}
	// Encoder restart: PTS regresses -> epoch 1 at the next IDR.
	_ = s.writeFrame(pkt(testAU(2000, true), 0, true))
	h, _ := reassemble(t, dc.take())
	if h.Epoch != 1 {
		t.Fatalf("epoch after restart = %d, want 1", h.Epoch)
	}
	wants.Store(0)
	// A request that names the old epoch is stale and ignored.
	s.handleFeedback(keyReq(0))
	if wants.Load() != 0 {
		t.Fatal("stale-epoch request must be ignored")
	}
	_ = s.writeFrame(pkt(testAU(500, false), 10*time.Millisecond, false))
	if len(dc.take()) == 0 {
		t.Fatal("stale request must not gate video")
	}
}

func TestSenderPrependsCachedParamSets(t *testing.T) {
	dc := &fakeDC{state: webrtc.DataChannelStateOpen}
	s, _, _ := newTestSender(VideoTransportWebCodecs, dc)
	s.handleFeedback(helloMsg(CapWebCodecsH264))
	_ = s.writeFrame(pkt(testAU(1500, true), 0, true))
	dc.take()
	s.handleFeedback(keyReq(0))
	// An IDR with no in-band SPS/PPS.
	bare := append([]byte{0, 0, 0, 1, 0x65}, bytes.Repeat([]byte{0xAB}, 400)...)
	_ = s.writeFrame(pkt(bare, 20*time.Millisecond, true))
	h, au := reassemble(t, dc.take())
	if h.Flags&FlagParam == 0 {
		t.Fatal("PARAM flag missing after prepend")
	}
	if _, sps, pps := paramSets(au); !sps || !pps {
		t.Fatal("cached SPS/PPS not prepended")
	}
}

func TestSenderTransportSwitch(t *testing.T) {
	dc := &fakeDC{state: webrtc.DataChannelStateOpen}
	s, wants, acks := newTestSender(VideoTransportAuto, dc)
	if s.Active() != VideoTransportTrack {
		t.Fatal("auto must start on track")
	}
	_ = s.writeFrame(pkt(testAU(2000, true), 0, true))
	if len(dc.take()) != 0 {
		t.Fatal("track-active viewer must not receive datachannel video")
	}
	s.handleFeedback(helloMsg(CapWebCodecsH264))
	if wants.Load() != 0 {
		t.Fatal("HELLO on a track viewer must not force an IDR")
	}
	if a, _ := s.switchTo("webcodecs"); a != VideoTransportWebCodecs {
		t.Fatal("switch to webcodecs refused")
	}
	if wants.Load() != 1 {
		t.Fatal("switch must force a keyframe")
	}
	_ = s.writeFrame(pkt(testAU(2000, true), 10*time.Millisecond, true))
	h, _ := reassemble(t, dc.take())
	if h.Epoch != 1 {
		t.Fatalf("switch must bump epoch, got %d", h.Epoch)
	}
	if a, toTrack := s.switchTo("track"); a != VideoTransportTrack || !toTrack {
		t.Fatal("switch back to track failed")
	}
	var last struct{ Kind, Active string }
	_ = json.Unmarshal((*acks)[len(*acks)-1], &last)
	if last.Kind != "video_transport" || last.Active != "track" {
		t.Fatalf("ack = %s", (*acks)[len(*acks)-1])
	}
	_ = s.writeFrame(pkt(testAU(2000, true), 20*time.Millisecond, true))
	if len(dc.take()) != 0 {
		t.Fatal("DataChannel video continued after switching to track")
	}

	// track mode refuses to leave the track.
	s2, _, _ := newTestSender(VideoTransportTrack, dc)
	if a, _ := s2.switchTo("webcodecs"); a != VideoTransportTrack {
		t.Fatal("track mode must stay on track")
	}
	// Viewer without WebCodecs support falls back automatically.
	s3, _, acks3 := newTestSender(VideoTransportWebCodecs, dc)
	s3.handleFeedback(helloMsg(0))
	if s3.Active() != VideoTransportTrack || len(*acks3) == 0 {
		t.Fatal("HELLO without WebCodecs caps must fall back to track")
	}
}

func TestSenderPacedLargeFrame(t *testing.T) {
	dc := &fakeDC{state: webrtc.DataChannelStateOpen}
	s := newVideoSender(videoSenderConfig{Mode: VideoTransportWebCodecs, BufferMax: 1 << 30, IDRPaceMS: 8})
	s.setChannel(dc)
	s.handleFeedback(helloMsg(CapWebCodecsH264))
	start := time.Now()
	_ = s.writeFrame(pkt(testAU(40*1172, true), 0, true)) // 40 fragments -> 5 bursts
	if len(dc.take()) < 40 {
		t.Fatal("not all fragments sent")
	}
	if time.Since(start) < 4*time.Millisecond {
		t.Fatalf("large frame not paced: %v", time.Since(start))
	}
}

// --- loopback with a pion answerer playing the browser ---

func TestWebCodecsLoopback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var wants atomic.Int32
	host, err := NewPeer(Config{
		CommandBuffer:          8,
		VideoTransport:         VideoTransportAuto,
		VideoIDRPaceMS:         -1,
		OnVideoKeyframeRequest: func() { wants.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()

	browser, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()

	var (
		videoDC, fbDC, ctrlDC *webrtc.DataChannel
		dcMu                  sync.Mutex
		ready                 = make(chan struct{}, 3)
		frags                 = make(chan []byte, 256)
		ctrlText              = make(chan string, 8)
	)
	browser.OnDataChannel(func(dc *webrtc.DataChannel) {
		dcMu.Lock()
		defer dcMu.Unlock()
		switch dc.Label() {
		case VideoChannelLabel:
			videoDC = dc
			dc.OnMessage(func(m webrtc.DataChannelMessage) {
				select {
				case frags <- append([]byte(nil), m.Data...):
				default:
				}
			})
		case VideoFeedbackChannelLabel:
			fbDC = dc
		case ControlChannelLabel:
			ctrlDC = dc
			dc.OnMessage(func(m webrtc.DataChannelMessage) {
				if m.IsString {
					select {
					case ctrlText <- string(m.Data):
					default:
					}
				}
			})
		default:
			return
		}
		ready <- struct{}{}
	})

	offer, err := host.CreateOffer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := browser.SetRemoteDescription(offer); err != nil {
		t.Fatal(err)
	}
	gather := webrtc.GatheringCompletePromise(browser)
	answer, err := browser.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := browser.SetLocalDescription(answer); err != nil {
		t.Fatal(err)
	}
	<-gather
	if err := host.SetRemoteDescription(*browser.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	waitConnected(ctx, t, host.ConnState())
	for i := 0; i < 3; i++ {
		select {
		case <-ready:
		case <-ctx.Done():
			t.Fatal("timeout waiting for data channels")
		}
	}
	dcMu.Lock()
	v, fb, ctrl := videoDC, fbDC, ctrlDC
	dcMu.Unlock()
	waitOpen(ctx, t, v)
	waitOpen(ctx, t, fb)
	waitOpen(ctx, t, ctrl)

	if host.VideoActive() != VideoTransportTrack {
		t.Fatal("auto must start on the track")
	}
	// Viewer opts into WebCodecs, then says HELLO.
	if err := ctrl.SendText(`{"kind":"video_transport","use":"webcodecs"}`); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-ctrlText:
		if s != `{"kind":"video_transport","active":"webcodecs"}` {
			t.Fatalf("ack = %s", s)
		}
	case <-ctx.Done():
		t.Fatal("no video_transport ack")
	}
	if err := fb.Send(helloMsg(CapWebCodecsH264 | CapAnnexB)); err != nil {
		t.Fatal(err)
	}
	waitFor(ctx, t, func() bool { return wants.Load() >= 1 })

	// Host sends an IDR large enough for several fragments; the answerer
	// reassembles it byte for byte.
	au := testAU(5000, true)
	if err := host.WriteSample(core.EncodedPacket{Data: au, PTS: 40 * time.Millisecond, IsKeyframe: true}, DefaultSampleDuration); err != nil {
		t.Fatal(err)
	}
	var msgs [][]byte
	want := FragmentCount(len(au), DefaultVideoFragmentBytes-VideoHeaderSize)
	for len(msgs) < want {
		select {
		case m := <-frags:
			msgs = append(msgs, m)
		case <-ctx.Done():
			t.Fatalf("got %d/%d fragments", len(msgs), want)
		}
	}
	// SCTP unordered delivery on loopback is normally in order; sort by index
	// to keep the test deterministic regardless.
	sorted := make([][]byte, want)
	for _, m := range msgs {
		h, _, _ := ParseVideoHeader(m)
		sorted[h.FragIdx] = m
	}
	h, out := reassemble(t, sorted)
	if !bytes.Equal(out, au) || h.PTSUs != 40000 || h.Flags != FlagKey|FlagParam {
		t.Fatalf("loopback frame mismatch: %+v equal=%v", h, bytes.Equal(out, au))
	}

	// KEYFRAME_REQUEST reaches the host callback; the RTP sink got nothing.
	before := wants.Load()
	if err := fb.Send(keyReq(h.Epoch)); err != nil {
		t.Fatal(err)
	}
	waitFor(ctx, t, func() bool { return wants.Load() > before })

	// ABR on this path: a FRAME_REPORT yields a valid webcodecs sample, and
	// Pion reports the SCTP smoothed RTT that queueing detection relies on.
	if err := fb.Send(frameReport(10, 0, 100, 100, 30)); err != nil {
		t.Fatal(err)
	}
	var sample StatsSample
	waitFor(ctx, t, func() bool { sample = host.Stats(); return sample.Valid })
	if sample.Source != "webcodecs" || sample.LossFraction != 0 || sample.Congested {
		t.Fatalf("webcodecs sample = %+v", sample)
	}
	if srtt := sctpSRTT(host.pc.GetStats()); srtt <= 0 || srtt > time.Second {
		t.Fatalf("SCTP SRTT = %v, want a positive sub-second value", srtt)
	}

	// Switch back to the track: acked, and DataChannel video stops.
	if err := ctrl.SendText(`{"kind":"video_transport","use":"track"}`); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-ctrlText:
		if s != `{"kind":"video_transport","active":"track"}` {
			t.Fatalf("ack = %s", s)
		}
	case <-ctx.Done():
		t.Fatal("no track ack")
	}
	if host.VideoActive() != VideoTransportTrack {
		t.Fatal("host still on webcodecs")
	}
}

func waitFor(ctx context.Context, t *testing.T, cond func() bool) {
	t.Helper()
	for !cond() {
		select {
		case <-ctx.Done():
			t.Fatal("timeout waiting for condition")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
