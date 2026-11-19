package rtc

import (
	"encoding/json"
	"fmt"

	"github.com/pion/webrtc/v4"
)

// VideoActive reports the viewer's current video sink: "track" or "webcodecs".
func (p *Peer) VideoActive() string {
	if p.vs == nil {
		return VideoTransportTrack
	}
	return p.vs.Active()
}

// LastVideoReport returns the most recent FRAME_REPORT from the viewer.
func (p *Peer) LastVideoReport() (FrameReport, bool) {
	if p.vs == nil {
		return FrameReport{}, false
	}
	return p.vs.lastReport()
}

// setupVideoChannels creates the "video" (unreliable, unordered) and "videofb"
// (reliable, ordered) channels when the configured transport is webcodecs or
// auto. In "track" mode no extra channels exist.
func (p *Peer) setupVideoChannels() error {
	mode := p.cfg.VideoTransport
	if mode != VideoTransportWebCodecs && mode != VideoTransportAuto {
		return nil
	}
	vs := newVideoSender(videoSenderConfig{
		Mode:          mode,
		FragmentBytes: p.cfg.VideoFragmentBytes,
		BufferMax:     uint64(p.cfg.VideoBufferMaxBytes),
		IDRPaceMS:     p.cfg.VideoIDRPaceMS,
		FEC:           ParseFECScheme(p.cfg.VideoFEC),
		FECRatio:      p.cfg.VideoFECRatio,
		Log:           p.log,
		WantKeyframe:  p.cfg.OnVideoKeyframeRequest,
		OnSwitchToTrack: func() {
			// Re-arm the RTP keyframe gate so the track starts on the next IDR.
			p.vmu.Lock()
			p.seenKeyframe = false
			p.vmu.Unlock()
		},
		SendAck: func(b []byte) {
			if err := p.SendControl(b); err != nil {
				p.log.Debug("video_transport ack failed", "err", err)
			}
		},
	})

	ordered := false
	var zero uint16
	video, err := p.pc.CreateDataChannel(VideoChannelLabel, &webrtc.DataChannelInit{
		Ordered:        &ordered,
		MaxRetransmits: &zero,
	})
	if err != nil {
		return fmt.Errorf("rtc: create video channel: %w", err)
	}
	video.OnOpen(func() {
		p.log.Info("video channel open")
		vs.onChannelOpen()
	})
	vs.setChannel(video)

	fb, err := p.pc.CreateDataChannel(VideoFeedbackChannelLabel, nil)
	if err != nil {
		return fmt.Errorf("rtc: create videofb channel: %w", err)
	}
	fb.OnOpen(func() { p.log.Info("videofb channel open") })
	fb.OnMessage(func(msg webrtc.DataChannelMessage) {
		if msg.IsString {
			return
		}
		vs.handleFeedback(msg.Data)
	})
	p.vs = vs
	return nil
}

// handleVideoTransport processes {"kind":"video_transport","use":"track|webcodecs"}.
// Hosts without the WebCodecs channels (mode "track") answer active=track.
func (p *Peer) handleVideoTransport(data []byte) {
	var msg struct {
		Use string `json:"use"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return
	}
	if p.vs == nil {
		_ = p.SendControl([]byte(`{"kind":"video_transport","active":"track"}`))
		return
	}
	p.vs.switchTo(msg.Use) // OnSwitchToTrack re-arms the RTP keyframe gate
}
