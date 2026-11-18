package rtc

import (
	"context"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// TestOnControlOpen checks that a callback registered before the control
// channel opens can send on it, and that one registered afterwards still runs.
func TestOnControlOpen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	host, err := NewPeer(Config{LoopbackOnly: true})
	if err != nil {
		t.Fatalf("NewPeer: %v", err)
	}
	defer host.Close()
	host.OnControlOpen(func() { _ = host.SendControl([]byte(`{"kind":"open"}`)) })

	browser, err := NewLoopbackPeerConnection()
	if err != nil {
		t.Fatalf("browser pc: %v", err)
	}
	defer browser.Close()
	got := make(chan string, 4)
	browser.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != ControlChannelLabel {
			return
		}
		dc.OnMessage(func(msg webrtc.DataChannelMessage) { got <- string(msg.Data) })
	})

	offer, err := host.CreateOffer(ctx)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	if err := browser.SetRemoteDescription(offer); err != nil {
		t.Fatalf("SetRemoteDescription: %v", err)
	}
	gather := webrtc.GatheringCompletePromise(browser)
	answer, err := browser.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("CreateAnswer: %v", err)
	}
	if err := browser.SetLocalDescription(answer); err != nil {
		t.Fatalf("SetLocalDescription: %v", err)
	}
	<-gather
	if err := host.SetRemoteDescription(*browser.LocalDescription()); err != nil {
		t.Fatalf("host SetRemoteDescription: %v", err)
	}

	select {
	case m := <-got:
		if m != `{"kind":"open"}` {
			t.Fatalf("got %q", m)
		}
	case <-ctx.Done():
		t.Fatal("open event not received")
	}

	late := make(chan struct{})
	host.OnControlOpen(func() { close(late) })
	select {
	case <-late:
	case <-ctx.Done():
		t.Fatal("late OnControlOpen did not run")
	}
}
