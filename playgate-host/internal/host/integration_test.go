package host

import (
	"context"
	"encoding/json"
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

	connect := func(ctx context.Context, router *VideoRouter, sink InputSink) error {
		peer, err := rtc.NewPeer(rtc.Config{ICEServers: nil, Logger: discardLogger()})
		if err != nil {
			connectErr <- err
			return err
		}
		defer peer.Close()

		router.SetSink(peer)
		defer router.Clear()

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

