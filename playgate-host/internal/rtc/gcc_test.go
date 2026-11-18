package rtc

import (
	"context"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/playgate/playgate-host/internal/core"
)

func TestTargetBitrateDisabled(t *testing.T) {
	p, err := NewPeer(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, ok := p.TargetBitrate(); ok {
		t.Fatal("TargetBitrate ok=true with GCC disabled")
	}
}

// TestLoopbackGCC enables GCC and verifies the estimator is exposed and reports
// a positive target bitrate once video flows through the interceptor chain.
func TestLoopbackGCC(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	host, err := NewPeer(Config{
		CommandBuffer:     8,
		EnableGCC:         true,
		GCCInitialBitrate: 2_000_000,
		GCCMinBitrate:     500_000,
		GCCMaxBitrate:     8_000_000,
	})
	if err != nil {
		t.Fatalf("NewPeer: %v", err)
	}
	defer host.Close()

	browser, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	browser.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			if _, _, err := tr.ReadRTP(); err != nil {
				return
			}
		}
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

	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		pts := time.Duration(0)
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

	deadline := time.Now().Add(10 * time.Second)
	for {
		if bps, ok := host.TargetBitrate(); ok && bps > 0 {
			t.Logf("target bitrate = %d bps", bps)
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("TargetBitrate never became ok with a positive value")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
