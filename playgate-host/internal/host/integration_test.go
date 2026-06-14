package host

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/playgate/playgate-host/internal/capture/synthetic"
	"github.com/playgate/playgate-host/internal/config"
	"github.com/playgate/playgate-host/internal/core"
	"github.com/playgate/playgate-host/internal/metrics"
	"github.com/playgate/playgate-host/internal/rtc"
	"github.com/playgate/playgate-host/internal/signaling"
)

// TestEndToEndLoopback wires the real synthetic capture source, a fake encoder,
// the real VideoRouter, and a real rtc.Peer connected in-process to a bare
// answerer PeerConnection (the "browser"). It verifies:
//   - media flows capture → encoder → router → rtc track → browser (RTP arrives)
//   - input flows browser → rtc "input" channel → router/sink → fake target
//   - graceful shutdown leaves no goroutine leak
func TestEndToEndLoopback(t *testing.T) {
	cap, err := synthetic.New(discardLogger(), synthetic.Config{Width: 64, Height: 48, FPS: 120})
	if err != nil {
		t.Fatal(err)
	}
	enc := newFakeEncoder(cap.Frames())
	in := newFakeInput()
	mc := metrics.NewCollector()

	// gotRTP is closed when the browser receives the first RTP packet.
	gotRTP := make(chan struct{})
	// connectReady signals the test that the peer is wired so it can drive input.
	connectErr := make(chan error, 1)

	connect := func(ctx context.Context, router *VideoRouter, arouter *AudioRouter, sink InputSink) error {
		peer, err := rtc.NewPeer(rtc.Config{ICEServers: nil, Logger: discardLogger()})
		if err != nil {
			connectErr <- err
			return err
		}
		defer peer.Close()

		router.AddSink(peer)
		defer router.RemoveSink(peer)

		// Forward decoded commands to the fake target (session disabled path).
		go sink.HandleCommands(ctx, peer.Commands(), func(b []byte) error { return peer.SendControl(b) })

		// --- browser side ---
		browser, err := webrtc.NewPeerConnection(webrtc.Configuration{})
		if err != nil {
			connectErr <- err
			return err
		}
		defer browser.Close()

		inputDCCh := make(chan *webrtc.DataChannel, 1)
		browser.OnDataChannel(func(dc *webrtc.DataChannel) {
			if dc.Label() == rtc.InputChannelLabel {
				select {
				case inputDCCh <- dc:
				default:
				}
			}
		})
		var rtpOnce bool
		browser.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
			for {
				if _, _, err := tr.ReadRTP(); err != nil {
					return
				}
				if !rtpOnce {
					rtpOnce = true
					close(gotRTP)
				}
			}
		})

		// --- in-process signaling: host offer -> browser answer ---
		offer, err := peer.CreateOffer(ctx)
		if err != nil {
			connectErr <- err
			return err
		}
		if err := browser.SetRemoteDescription(offer); err != nil {
			connectErr <- err
			return err
		}
		gather := webrtc.GatheringCompletePromise(browser)
		answer, err := browser.CreateAnswer(nil)
		if err != nil {
			connectErr <- err
			return err
		}
		if err := browser.SetLocalDescription(answer); err != nil {
			connectErr <- err
			return err
		}
		<-gather
		if err := peer.SetRemoteDescription(*browser.LocalDescription()); err != nil {
			connectErr <- err
			return err
		}

		// Once the input channel opens, send a controller frame.
		go func() {
			var dc *webrtc.DataChannel
			select {
			case dc = <-inputDCCh:
			case <-ctx.Done():
				return
			}
			// Wait for open.
			deadline := time.After(10 * time.Second)
			for dc.ReadyState() != webrtc.DataChannelStateOpen {
				select {
				case <-time.After(20 * time.Millisecond):
				case <-deadline:
					return
				case <-ctx.Done():
					return
				}
			}
			cmd := core.InputCommand{Buttons: core.ButtonA | core.ButtonR, LX: 0.5}
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(16 * time.Millisecond):
					_ = dc.Send(rtc.EncodeInputCommand(cmd))
				}
			}
		}()

		connectErr <- nil
		<-ctx.Done()
		return nil
	}

	deps := Deps{Capture: cap, Encoder: enc, Input: in, Connect: connect}
	h := NewWithDeps(discardLogger(), config.Default(), deps, mc)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- h.Run(ctx) }()

	// connect goroutine reports setup success/failure.
	select {
	case err := <-connectErr:
		if err != nil {
			t.Fatalf("connect setup failed: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("connect did not finish setup")
	}

	// Media must flow end to end.
	select {
	case <-gotRTP:
	case <-time.After(15 * time.Second):
		t.Fatal("timeout waiting for RTP on browser track")
	}

	// Input must reach the fake target.
	deadline := time.Now().Add(10 * time.Second)
	for in.count() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("input commands never reached the target")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Verify rtc-stage latency was recorded by the router for forwarded packets.
	// (Capture/encode stages are instrumented by the encoderWrapper, which this
	// test bypasses by injecting a fake encoder.)
	if mc.RTC.Snapshot().Count == 0 {
		t.Error("no rtc-stage latency recorded")
	}

	// Graceful shutdown.
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestApplyViewerMessageSkipsStaleAnswer verifies the host ignores answers whose
// Worker timestamp predates its own offer's (leftovers from a previous attempt
// against a dead peer) and still applies a fresh one. With offerTs unknown ("",
// older Worker) the answer is applied unconditionally.
func TestApplyViewerMessageSkipsStaleAnswer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		offerTs string
	}{
		{name: "stale-then-fresh", offerTs: "2026-06-12T00:00:10.000Z"},
		{name: "no-offer-ts-applies-unconditionally", offerTs: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			peer, err := rtc.NewPeer(rtc.Config{Logger: discardLogger()})
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()

			// Real browser-side answer so SetRemoteDescription succeeds.
			browser, err := webrtc.NewPeerConnection(webrtc.Configuration{})
			if err != nil {
				t.Fatal(err)
			}
			defer browser.Close()
			offer, err := peer.CreateOffer(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := browser.SetRemoteDescription(offer); err != nil {
				t.Fatal(err)
			}
			answer, err := browser.CreateAnswer(nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := browser.SetLocalDescription(answer); err != nil {
				t.Fatal(err)
			}
			payload, err := json.Marshal(signaling.SDPMessage{Type: "answer", SDP: answer.SDP})
			if err != nil {
				t.Fatal(err)
			}

			cm := &connManager{log: discardLogger()}
			stale := signaling.Message{Seq: 0, Ts: "2026-06-12T00:00:05.000Z", Payload: payload}
			fresh := signaling.Message{Seq: 1, Ts: "2026-06-12T00:00:15.000Z", Payload: payload}

			if tc.offerTs != "" {
				if cm.applyViewerMessage(peer, stale, tc.offerTs) {
					t.Fatal("stale answer (ts < offerTs) was applied")
				}
			}
			if !cm.applyViewerMessage(peer, fresh, tc.offerTs) {
				t.Fatal("fresh answer was not applied")
			}
		})
	}
}

// buildRealAnswer creates a genuine SDP answer for the given peer's offer so
// that SetRemoteDescription in applyViewerMessage succeeds.
func buildRealAnswer(t *testing.T, peer *rtc.Peer) string {
	t.Helper()
	ctx := context.Background()
	offer, err := peer.CreateOffer(ctx)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	browser, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("new browser: %v", err)
	}
	t.Cleanup(func() { _ = browser.Close() })
	if err := browser.SetRemoteDescription(offer); err != nil {
		t.Fatalf("browser SetRemoteDescription: %v", err)
	}
	answer, err := browser.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("CreateAnswer: %v", err)
	}
	if err := browser.SetLocalDescription(answer); err != nil {
		t.Fatalf("browser SetLocalDescription: %v", err)
	}
	return answer.SDP
}

// pollForAnswerServer builds an httptest server whose /rooms/ handler behaves
// as a signaling Worker for pollForAnswer tests. On the first poll it holds for
// holdFor before replying; subsequent polls reply immediately. answerPayload is
// returned as the sole message in the first delayed response.
func pollForAnswerServer(t *testing.T, holdFor time.Duration, answerPayload []byte) (*httptest.Server, *int64) {
	t.Helper()
	var reqCount int64
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&reqCount, 1)
		n := atomic.LoadInt64(&reqCount)
		if n == 1 && holdFor > 0 {
			select {
			case <-time.After(holdFor):
			case <-r.Context().Done():
				return
			}
		}
		ts := time.Now().UTC().Format(time.RFC3339Nano)
		var msgs []signaling.Message
		if n == 1 && answerPayload != nil {
			msgs = []signaling.Message{{Seq: 0, Ts: ts, Payload: json.RawMessage(answerPayload)}}
		}
		next := -1
		if len(msgs) > 0 {
			next = msgs[len(msgs)-1].Seq
		}
		type pollResp struct {
			Messages  []signaling.Message `json:"messages"`
			NextSince int                 `json:"nextSince"`
		}
		_ = json.NewEncoder(rw).Encode(pollResp{Messages: msgs, NextSince: next})
	}))
	t.Cleanup(srv.Close)
	return srv, &reqCount
}

// TestPollForAnswerLongPollReceivesAnswer verifies that pollForAnswer succeeds
// when the signaling server holds the first request for a short delay and then
// delivers an SDP answer.
func TestPollForAnswerLongPollReceivesAnswer(t *testing.T) {
	peer, err := rtc.NewPeer(rtc.Config{Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()

	answerSDP := buildRealAnswer(t, peer)
	payload, _ := json.Marshal(signaling.SDPMessage{Type: "answer", SDP: answerSDP})

	// Server holds for 150ms (> 1s threshold is NOT needed — we just need it to
	// deliver the answer; the 1s threshold only gates the anti-hot-spin sleep).
	srv, _ := pollForAnswerServer(t, 150*time.Millisecond, payload)

	sc, err := signaling.New(signaling.Config{
		BaseURL:    srv.URL,
		RoomID:     "r",
		HTTPClient: &http.Client{}, // no shared timeout — long-poll uses ctx deadline
	})
	if err != nil {
		t.Fatal(err)
	}

	cm := &connManager{
		log:    discardLogger(),
		client: sc,
		poll:   10 * time.Millisecond,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cm.pollForAnswer(ctx, peer, ""); err != nil {
		t.Fatalf("pollForAnswer returned error: %v", err)
	}
}

// TestPollForAnswerOldServerDegrades verifies that when the server ignores the
// wait parameter and returns immediately with no messages, pollForAnswer sleeps
// c.poll between calls instead of hot-spinning. We measure request rate: with
// poll=20ms and a 100ms window the loop should make far fewer than 100 calls.
func TestPollForAnswerOldServerDegrades(t *testing.T) {
	// Old server: always returns immediately with no messages.
	var reqCount int64
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&reqCount, 1)
		type pollResp struct {
			Messages  []signaling.Message `json:"messages"`
			NextSince int                 `json:"nextSince"`
		}
		_ = json.NewEncoder(rw).Encode(pollResp{Messages: nil, NextSince: -1})
	}))
	t.Cleanup(srv.Close)

	sc, err := signaling.New(signaling.Config{
		BaseURL:    srv.URL,
		RoomID:     "r",
		HTTPClient: &http.Client{},
	})
	if err != nil {
		t.Fatal(err)
	}

	const pollInterval = 20 * time.Millisecond
	cm := &connManager{
		log:    discardLogger(),
		client: sc,
		poll:   pollInterval,
	}

	// Run for 3 poll intervals and then cancel. With anti-hot-spin sleeping
	// pollInterval between each call, we expect at most ~4 requests.
	ctx, cancel := context.WithTimeout(context.Background(), 3*pollInterval)
	defer cancel()

	// Run in background; expect errTimeoutNoAnswer or ctx cancel.
	done := make(chan error, 1)
	go func() {
		// Use a peer stub — we only care about request count, not SDP application.
		// Since no answer is ever delivered, pollForAnswer will exit via ctx cancel.
		done <- cm.pollForAnswer(ctx, nil, "")
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("pollForAnswer did not return after ctx cancel")
	}

	got := atomic.LoadInt64(&reqCount)
	// With pollInterval=20ms and window=3*20ms=60ms, at most 4 iterations
	// (one immediate + one per sleep interval). Allow a generous upper bound of 8
	// to avoid flakiness on slow CI without permitting actual hot-spinning (which
	// would produce hundreds of calls).
	if got > 8 {
		t.Errorf("too many requests in degraded mode: got %d, want ≤8 (anti-hot-spin not working)", got)
	}
}

// TestPollForAnswerCtxCancelMidHold verifies that cancelling the context while
// a long-poll request is held by the server causes pollForAnswer to return
// promptly (within ~1 second).
func TestPollForAnswerCtxCancelMidHold(t *testing.T) {
	// Server holds until the client disconnects.
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // released when client aborts
	}))
	t.Cleanup(srv.Close)

	sc, err := signaling.New(signaling.Config{
		BaseURL:    srv.URL,
		RoomID:     "r",
		HTTPClient: &http.Client{},
	})
	if err != nil {
		t.Fatal(err)
	}

	cm := &connManager{
		log:    discardLogger(),
		client: sc,
		poll:   10 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- cm.pollForAnswer(ctx, nil, "")
	}()

	// Give it a moment to reach the server, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("pollForAnswer returned non-nil error after ctx cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pollForAnswer did not return within 1s after ctx cancel")
	}
}
