package rtc

import (
	"context"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/playgate/playgate-host/internal/core"
)

// TestLoopback wires a host-side Peer (offerer: video track + input/control
// channels) to a bare answerer PeerConnection that plays the role of the browser.
// It verifies that:
//   - SDP offer/answer round-trips through Encode/DecodeSDP,
//   - the connection reaches Connected,
//   - a binary controller frame sent on the "input" channel arrives decoded on
//     Peer.Commands,
//   - an H.264 keyframe sample written to the video track is received as RTP.
func TestLoopback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	fixedNow := time.Unix(1700000000, 0)

	// Host side: our Peer. Use the same default STUN config but ICE will complete
	// over host candidates on loopback.
	host, err := NewPeer(Config{
		ICEServers:    nil, // host-local candidates suffice in-process
		CommandBuffer: 8,
		now:           func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("NewPeer: %v", err)
	}
	defer host.Close()

	// Browser side: a plain PeerConnection (answerer).
	browser, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("browser pc: %v", err)
	}
	defer browser.Close()

	// Browser receives the in-band data channels and the video track.
	inputDCCh := make(chan *webrtc.DataChannel, 1)
	browser.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() == InputChannelLabel {
			select {
			case inputDCCh <- dc:
			default:
			}
		}
	})

	gotRTP := make(chan struct{}, 1)
	browser.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			if _, _, err := tr.ReadRTP(); err != nil {
				return
			}
			select {
			case gotRTP <- struct{}{}:
			default:
			}
		}
	})

	// --- Signaling: host offer -> browser answer (via base64 SDP) ---
	offer, err := host.CreateOffer(ctx)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	offerB64, err := EncodeSDP(offer)
	if err != nil {
		t.Fatalf("EncodeSDP: %v", err)
	}
	decodedOffer, err := DecodeSDP(offerB64)
	if err != nil {
		t.Fatalf("DecodeSDP: %v", err)
	}
	if err := browser.SetRemoteDescription(decodedOffer); err != nil {
		t.Fatalf("browser SetRemoteDescription: %v", err)
	}

	browserGather := webrtc.GatheringCompletePromise(browser)
	answer, err := browser.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("browser CreateAnswer: %v", err)
	}
	if err := browser.SetLocalDescription(answer); err != nil {
		t.Fatalf("browser SetLocalDescription: %v", err)
	}
	select {
	case <-browserGather:
	case <-ctx.Done():
		t.Fatal("timeout gathering browser candidates")
	}
	answerB64, err := EncodeSDP(*browser.LocalDescription())
	if err != nil {
		t.Fatalf("EncodeSDP answer: %v", err)
	}
	decodedAnswer, err := DecodeSDP(answerB64)
	if err != nil {
		t.Fatalf("DecodeSDP answer: %v", err)
	}
	if err := host.SetRemoteDescription(decodedAnswer); err != nil {
		t.Fatalf("host SetRemoteDescription: %v", err)
	}

	// --- Wait for connection ---
	waitConnected(ctx, t, host.ConnState())

	// --- Input channel: browser -> host ---
	var inputDC *webrtc.DataChannel
	select {
	case inputDC = <-inputDCCh:
	case <-ctx.Done():
		t.Fatal("timeout waiting for input data channel")
	}
	waitOpen(ctx, t, inputDC)

	sent := core.InputCommand{Buttons: core.ButtonA | core.ButtonR, LX: 0.5, LY: -0.5}
	if err := inputDC.Send(EncodeInputCommand(sent)); err != nil {
		t.Fatalf("send input frame: %v", err)
	}
	select {
	case got := <-host.Commands():
		if got.Buttons != sent.Buttons {
			t.Errorf("buttons = %#x, want %#x", got.Buttons, sent.Buttons)
		}
		if !got.Timestamp.Equal(fixedNow) {
			t.Errorf("timestamp = %v, want %v (injected clock)", got.Timestamp, fixedNow)
		}
		const eps = 2.0 / AxisScale
		if d := got.LX - sent.LX; d > eps || d < -eps {
			t.Errorf("LX = %v, want ~%v", got.LX, sent.LX)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for decoded command on host.Commands")
	}

	// --- Video: host -> browser. Keyframe gating means a non-keyframe is dropped
	// until the first keyframe arrives. Feed a keyframe then deltas. ---
	keyframe := core.EncodedPacket{Data: minimalH264Keyframe(), PTS: 0, IsKeyframe: true}
	if err := host.WriteSample(keyframe, DefaultSampleDuration); err != nil {
		t.Fatalf("WriteSample keyframe: %v", err)
	}
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		pts := 33 * time.Millisecond
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = host.WriteSample(core.EncodedPacket{Data: minimalH264Keyframe(), PTS: pts, IsKeyframe: true}, DefaultSampleDuration)
				pts += 33 * time.Millisecond
			}
		}
	}()

	select {
	case <-gotRTP:
		// success: media flowed end to end.
	case <-ctx.Done():
		t.Fatal("timeout waiting for RTP on browser track")
	}
}

// TestAudioTrackGating verifies the Opus track is created only when EnableAudio
// is set, and that WriteAudioSample is a safe no-op on a video-only peer.
func TestAudioTrackGating(t *testing.T) {
	videoOnly, err := NewPeer(Config{ICEServers: nil})
	if err != nil {
		t.Fatalf("NewPeer (video-only): %v", err)
	}
	defer videoOnly.Close()
	if videoOnly.AudioTrack() != nil {
		t.Fatal("AudioTrack should be nil when EnableAudio is false")
	}
	if err := videoOnly.WriteAudioSample([]byte{0x00}, 20*time.Millisecond); err != nil {
		t.Fatalf("WriteAudioSample on video-only peer should be a no-op, got %v", err)
	}

	withAudio, err := NewPeer(Config{ICEServers: nil, EnableAudio: true})
	if err != nil {
		t.Fatalf("NewPeer (audio): %v", err)
	}
	defer withAudio.Close()
	if withAudio.AudioTrack() == nil {
		t.Fatal("AudioTrack should be non-nil when EnableAudio is true")
	}
}

// TestLoopbackAudio wires an audio-enabled host Peer to a bare answerer and
// verifies the Opus track negotiates and that an Opus sample written to it is
// received as RTP on the browser side's audio track.
func TestLoopbackAudio(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	host, err := NewPeer(Config{ICEServers: nil, EnableAudio: true})
	if err != nil {
		t.Fatalf("NewPeer: %v", err)
	}
	defer host.Close()

	browser, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("browser pc: %v", err)
	}
	defer browser.Close()

	gotAudioRTP := make(chan struct{}, 1)
	browser.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		// Only the audio track should signal success; drain video too so its
		// receiver does not stall.
		isAudio := tr.Kind() == webrtc.RTPCodecTypeAudio
		for {
			if _, _, err := tr.ReadRTP(); err != nil {
				return
			}
			if isAudio {
				select {
				case gotAudioRTP <- struct{}{}:
				default:
				}
			}
		}
	})

	// --- Signaling handshake ---
	offer, err := host.CreateOffer(ctx)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	if err := browser.SetRemoteDescription(offer); err != nil {
		t.Fatalf("browser SetRemoteDescription: %v", err)
	}
	browserGather := webrtc.GatheringCompletePromise(browser)
	answer, err := browser.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("browser CreateAnswer: %v", err)
	}
	if err := browser.SetLocalDescription(answer); err != nil {
		t.Fatalf("browser SetLocalDescription: %v", err)
	}
	select {
	case <-browserGather:
	case <-ctx.Done():
		t.Fatal("timeout gathering browser candidates")
	}
	if err := host.SetRemoteDescription(*browser.LocalDescription()); err != nil {
		t.Fatalf("host SetRemoteDescription: %v", err)
	}

	waitConnected(ctx, t, host.ConnState())

	// Feed Opus samples until RTP is observed on the browser's audio track. The
	// payload bytes are arbitrary: RTP carries them verbatim, which is all the
	// transport-level assertion needs.
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = host.WriteAudioSample([]byte{0xf8, 0xff, 0xfe}, 20*time.Millisecond)
			}
		}
	}()

	select {
	case <-gotAudioRTP:
		// success: Opus media flowed end to end on its own track.
	case <-ctx.Done():
		t.Fatal("timeout waiting for audio RTP on browser track")
	}
}

// TestKeyframeGating verifies WriteSample drops packets until the first keyframe.
func TestKeyframeGating(t *testing.T) {
	p, err := NewPeer(Config{ICEServers: nil})
	if err != nil {
		t.Fatalf("NewPeer: %v", err)
	}
	defer p.Close()

	// Delta before any keyframe: dropped, no error, seenKeyframe stays false.
	if err := p.WriteSample(core.EncodedPacket{Data: []byte{0}, IsKeyframe: false}, DefaultSampleDuration); err != nil {
		t.Fatalf("pre-keyframe delta should be silently dropped, got %v", err)
	}
	if p.seenKeyframe {
		t.Fatal("seenKeyframe should still be false after dropping a delta")
	}
	// Keyframe flips the gate.
	if err := p.WriteSample(core.EncodedPacket{Data: minimalH264Keyframe(), IsKeyframe: true}, DefaultSampleDuration); err != nil {
		t.Fatalf("keyframe write: %v", err)
	}
	if !p.seenKeyframe {
		t.Fatal("seenKeyframe should be true after a keyframe")
	}
}

func waitConnected(ctx context.Context, t *testing.T, states <-chan webrtc.PeerConnectionState) {
	t.Helper()
	for {
		select {
		case s := <-states:
			switch s {
			case webrtc.PeerConnectionStateConnected:
				return
			case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
				t.Fatalf("connection reached terminal state %s before Connected", s)
			}
		case <-ctx.Done():
			t.Fatal("timeout waiting for Connected")
		}
	}
}

func waitOpen(ctx context.Context, t *testing.T, dc *webrtc.DataChannel) {
	t.Helper()
	if dc.ReadyState() == webrtc.DataChannelStateOpen {
		return
	}
	opened := make(chan struct{})
	dc.OnOpen(func() { close(opened) })
	// Re-check in case it opened between the guard and registering the handler.
	if dc.ReadyState() == webrtc.DataChannelStateOpen {
		return
	}
	select {
	case <-opened:
	case <-ctx.Done():
		t.Fatal("timeout waiting for data channel open")
	}
}
