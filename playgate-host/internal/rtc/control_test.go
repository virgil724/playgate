package rtc

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// TestControlPingEcho verifies the host echoes a control-channel ping back as a
// pong, preserving the original ts so the browser can compute RTT, and that
// non-ping control messages are delivered to the OnControlMessage callback.
func TestControlPingEcho(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	host, err := NewPeer(Config{ICEServers: nil})
	if err != nil {
		t.Fatalf("NewPeer: %v", err)
	}
	defer host.Close()

	gotAuth := make(chan []byte, 1)
	host.OnControlMessage(func(b []byte) {
		select {
		case gotAuth <- b:
		default:
		}
	})

	browser, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("browser pc: %v", err)
	}
	defer browser.Close()

	controlCh := make(chan *webrtc.DataChannel, 1)
	browser.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() == ControlChannelLabel {
			select {
			case controlCh <- dc:
			default:
			}
		}
	})

	// Signaling.
	offer, err := host.CreateOffer(ctx)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	if err := browser.SetRemoteDescription(offer); err != nil {
		t.Fatalf("browser SetRemoteDescription: %v", err)
	}
	gather := webrtc.GatheringCompletePromise(browser)
	answer, err := browser.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("CreateAnswer: %v", err)
	}
	if err := browser.SetLocalDescription(answer); err != nil {
		t.Fatalf("browser SetLocalDescription: %v", err)
	}
	select {
	case <-gather:
	case <-ctx.Done():
		t.Fatal("gather timeout")
	}
	if err := host.SetRemoteDescription(*browser.LocalDescription()); err != nil {
		t.Fatalf("host SetRemoteDescription: %v", err)
	}

	var control *webrtc.DataChannel
	select {
	case control = <-controlCh:
	case <-ctx.Done():
		t.Fatal("no control channel")
	}
	waitOpen(ctx, t, control)

	// Collect pongs.
	pongCh := make(chan map[string]any, 1)
	control.OnMessage(func(msg webrtc.DataChannelMessage) {
		if !msg.IsString {
			return
		}
		var m map[string]any
		if err := json.Unmarshal(msg.Data, &m); err == nil {
			if m["kind"] == "pong" {
				select {
				case pongCh <- m:
				default:
				}
			}
		}
	})

	// Send a ping with a ts.
	if err := control.SendText(`{"kind":"ping","ts":12345}`); err != nil {
		t.Fatalf("send ping: %v", err)
	}
	select {
	case m := <-pongCh:
		if ts, ok := m["ts"].(float64); !ok || ts != 12345 {
			t.Errorf("pong ts = %v, want 12345", m["ts"])
		}
	case <-ctx.Done():
		t.Fatal("no pong received")
	}

	// Send a non-ping control message; it should reach OnControlMessage.
	if err := control.SendText(`{"kind":"auth","token":"abc"}`); err != nil {
		t.Fatalf("send auth: %v", err)
	}
	select {
	case b := <-gotAuth:
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if m["kind"] != "auth" {
			t.Errorf("callback got kind %v, want auth", m["kind"])
		}
	case <-ctx.Done():
		t.Fatal("auth message not delivered to callback")
	}
}
