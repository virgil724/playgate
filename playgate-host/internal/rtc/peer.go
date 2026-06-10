// Package rtc implements the PlayGate Host WebRTC layer on top of Pion. A single
// PeerConnection carries everything: an H.264 video MediaTrack pushed to the
// viewer, an unreliable/unordered "input" DataChannel that delivers controller
// commands back to the host, and a reliable "control" DataChannel reserved for
// session-level messages (T9).
//
// The package is signaling-agnostic. It exposes raw offer/answer SDP exchange so
// that callers can wire it to manual copy-paste (cmd/rtctest), a Cloudflare
// Worker (T7), or an in-process loopback (tests).
//
// Channel ownership follows the core golden rule: Peer owns and closes the
// Commands and ConnState channels when Close is called or Run returns.
package rtc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/playgate/playgate-host/internal/core"
)

// DataChannel labels. The browser side (T11) must use the exact same labels.
const (
	// InputChannelLabel is the unreliable/unordered controller command channel.
	InputChannelLabel = "input"
	// ControlChannelLabel is the reliable session-control channel (T9).
	ControlChannelLabel = "control"
)

// Default video track identifiers. streamID groups tracks belonging to the same
// logical media stream on the browser side.
const (
	videoTrackID  = "video"
	videoStreamID = "playgate"
)

// Config configures a Peer. The zero value is not valid; use DefaultConfig and
// override fields as needed.
type Config struct {
	// ICEServers is the list of STUN/TURN server URLs (and optional credentials).
	// TURN credentials (T8) plug straight in here via Username/Credential.
	ICEServers []webrtc.ICEServer

	// Logger receives structured diagnostics. If nil, a discard logger is used.
	Logger *slog.Logger

	// CommandBuffer is the buffer size of the Commands channel. A small buffer
	// absorbs bursts without blocking the SCTP read goroutine; commands are the
	// latest-state snapshots so dropping the oldest on overflow is acceptable.
	CommandBuffer int

	// now returns the current time; injectable for deterministic tests. Nil means
	// time.Now.
	now func() time.Time
}

// DefaultConfig returns a Config with a public Google STUN server and sensible
// buffer sizes. ICEServers is made configurable so T8 can inject TURN later.
func DefaultConfig() Config {
	return Config{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
		CommandBuffer: 64,
	}
}

// ICEServersFromURLs builds a []webrtc.ICEServer from plain URL strings (the form
// stored in config.WebRTCConfig.ICEServers). Empty input yields a nil slice.
func ICEServersFromURLs(urls []string) []webrtc.ICEServer {
	if len(urls) == 0 {
		return nil
	}
	return []webrtc.ICEServer{{URLs: append([]string(nil), urls...)}}
}

// Peer wraps one Pion PeerConnection and the PlayGate-specific tracks/channels.
//
// Lifecycle: construct with NewPeer, exchange SDP via CreateOffer/CreateAnswer +
// SetRemoteDescription, feed video with WriteSample (or run the packet pump via
// Run), read controller input from Commands and connection state from ConnState.
// Call Close exactly once to tear everything down.
type Peer struct {
	cfg   Config
	log   *slog.Logger
	now   func() time.Time
	pc    *webrtc.PeerConnection
	video *webrtc.TrackLocalStaticSample

	// control is the reliable channel; populated either on create (offerer) or on
	// OnDataChannel (answerer). Guarded by mu. closed guards the owned channels
	// against sends racing with Close (Pion fires state callbacks asynchronously).
	mu      sync.Mutex
	control *webrtc.DataChannel
	closed  bool

	commands  chan core.InputCommand
	connState chan webrtc.PeerConnectionState

	// keyframe gating: WriteSample drops packets until the first keyframe is seen
	// so a freshly-attached decoder starts on an IDR.
	seenKeyframe bool

	closeOnce sync.Once
}

// NewPeer creates a PeerConnection, registers the H.264 video track, and opens
// the input/control DataChannels (this side is the offerer/creator).
//
// The browser, as answerer, receives both channels in-band via OnDataChannel and
// does not create them itself.
func NewPeer(cfg Config) (*Peer, error) {
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	now := cfg.now
	if now == nil {
		now = time.Now
	}
	if cfg.CommandBuffer <= 0 {
		cfg.CommandBuffer = 64
	}

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: cfg.ICEServers})
	if err != nil {
		return nil, fmt.Errorf("rtc: create peer connection: %w", err)
	}

	p := &Peer{
		cfg:       cfg,
		log:       log,
		now:       now,
		pc:        pc,
		commands:  make(chan core.InputCommand, cfg.CommandBuffer),
		connState: make(chan webrtc.PeerConnectionState, 8),
	}

	if err := p.setupVideoTrack(); err != nil {
		_ = pc.Close()
		return nil, err
	}
	if err := p.setupDataChannels(); err != nil {
		_ = pc.Close()
		return nil, err
	}
	p.wireConnState()

	return p, nil
}

// setupVideoTrack creates the H.264 sample track and adds it to the connection.
func (p *Peer) setupVideoTrack() error {
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
		videoTrackID, videoStreamID,
	)
	if err != nil {
		return fmt.Errorf("rtc: create video track: %w", err)
	}
	if _, err := p.pc.AddTrack(track); err != nil {
		return fmt.Errorf("rtc: add video track: %w", err)
	}
	p.video = track
	return nil
}

// setupDataChannels opens the unreliable "input" and reliable "control" channels.
func (p *Peer) setupDataChannels() error {
	// input: ordered=false, maxRetransmits=0 -> unreliable, unordered. A dropped
	// or reordered controller snapshot is simply ignored; freshness beats delivery.
	ordered := false
	var zeroRetransmits uint16 = 0
	input, err := p.pc.CreateDataChannel(InputChannelLabel, &webrtc.DataChannelInit{
		Ordered:        &ordered,
		MaxRetransmits: &zeroRetransmits,
	})
	if err != nil {
		return fmt.Errorf("rtc: create input channel: %w", err)
	}
	p.attachInputChannel(input)

	// control: reliable + ordered (defaults) for session-level messages (T9).
	control, err := p.pc.CreateDataChannel(ControlChannelLabel, nil)
	if err != nil {
		return fmt.Errorf("rtc: create control channel: %w", err)
	}
	p.mu.Lock()
	p.control = control
	p.mu.Unlock()
	control.OnOpen(func() { p.log.Info("control channel open") })

	return nil
}

// attachInputChannel registers the message handler that decodes controller frames.
func (p *Peer) attachInputChannel(dc *webrtc.DataChannel) {
	dc.OnOpen(func() { p.log.Info("input channel open") })
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if msg.IsString {
			// JSON fallback is not implemented; ignore text frames for now.
			p.log.Debug("ignoring text input frame", "len", len(msg.Data))
			return
		}
		cmd, err := DecodeInputCommand(msg.Data, p.now())
		if err != nil {
			p.log.Warn("decode input frame", "err", err)
			return
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.closed {
			return
		}
		select {
		case p.commands <- cmd:
		default:
			// Buffer full: drop the oldest by reading one then pushing. Best effort;
			// the freshest state still wins.
			select {
			case <-p.commands:
			default:
			}
			select {
			case p.commands <- cmd:
			default:
			}
		}
	})
}

// wireConnState forwards Pion connection-state changes onto connState and also
// handles the answerer case where DataChannels arrive via OnDataChannel.
func (p *Peer) wireConnState() {
	p.pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		p.log.Info("connection state", "state", s.String())
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.closed {
			return
		}
		select {
		case p.connState <- s:
		default:
		}
	})

	// If this Peer is used as an answerer (browser/loopback offers the channels),
	// adopt the incoming channels by label.
	p.pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		switch dc.Label() {
		case InputChannelLabel:
			p.attachInputChannel(dc)
		case ControlChannelLabel:
			p.mu.Lock()
			p.control = dc
			p.mu.Unlock()
			dc.OnOpen(func() { p.log.Info("control channel open (remote)") })
		default:
			p.log.Debug("ignoring unknown data channel", "label", dc.Label())
		}
	})
}

// Commands returns the receive-only channel of decoded controller commands. Peer
// owns and closes it on Close.
func (p *Peer) Commands() <-chan core.InputCommand { return p.commands }

// ConnState returns the receive-only channel of PeerConnection state changes.
// Peer owns and closes it on Close.
func (p *Peer) ConnState() <-chan webrtc.PeerConnectionState { return p.connState }

// VideoTrack exposes the underlying sample track (mainly for tests).
func (p *Peer) VideoTrack() *webrtc.TrackLocalStaticSample { return p.video }

// SendControl sends a reliable message on the control channel. It returns an
// error if the channel is not yet open.
func (p *Peer) SendControl(data []byte) error {
	p.mu.Lock()
	dc := p.control
	p.mu.Unlock()
	if dc == nil {
		return errors.New("rtc: control channel not ready")
	}
	return dc.Send(data)
}

// WriteSample pushes one encoded H.264 packet onto the video track. duration is
// the presentation duration of this access unit (typically the delta between
// consecutive PTS values).
//
// Keyframe gating: packets are dropped until the first keyframe is observed so a
// newly-attached decoder always begins on an IDR. Once a keyframe has been seen,
// every packet flows.
//
// TODO(T4): wire RTCP PLI handling. When the viewer's RTCP receiver requests a
// keyframe (Picture Loss Indication) we should reset seenKeyframe and ask the
// encoder for an IDR. The encoder PLI hook lands with T3 integration.
func (p *Peer) WriteSample(pkt core.EncodedPacket, duration time.Duration) error {
	if !p.seenKeyframe {
		if !pkt.IsKeyframe {
			return nil // wait for the first IDR
		}
		p.seenKeyframe = true
	}
	return p.video.WriteSample(media.Sample{
		Data:     pkt.Data,
		Duration: duration,
	})
}

// CreateOffer generates a local offer, sets it as the local description, and
// blocks until ICE gathering completes so the returned SDP is fully populated
// (non-trickle). Use Encode to serialise the result for manual signaling.
func (p *Peer) CreateOffer(ctx context.Context) (webrtc.SessionDescription, error) {
	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("rtc: create offer: %w", err)
	}
	return p.gatherLocal(ctx, offer)
}

// CreateAnswer generates a local answer (after SetRemoteDescription) and blocks
// until ICE gathering completes.
func (p *Peer) CreateAnswer(ctx context.Context) (webrtc.SessionDescription, error) {
	answer, err := p.pc.CreateAnswer(nil)
	if err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("rtc: create answer: %w", err)
	}
	return p.gatherLocal(ctx, answer)
}

// gatherLocal sets desc as the local description and waits for ICE gathering to
// finish (or ctx to be cancelled), returning the gathered local description.
func (p *Peer) gatherLocal(ctx context.Context, desc webrtc.SessionDescription) (webrtc.SessionDescription, error) {
	gatherComplete := webrtc.GatheringCompletePromise(p.pc)
	if err := p.pc.SetLocalDescription(desc); err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("rtc: set local description: %w", err)
	}
	select {
	case <-gatherComplete:
	case <-ctx.Done():
		return webrtc.SessionDescription{}, ctx.Err()
	}
	local := p.pc.LocalDescription()
	if local == nil {
		return webrtc.SessionDescription{}, errors.New("rtc: nil local description after gathering")
	}
	return *local, nil
}

// SetRemoteDescription applies the peer's offer or answer.
func (p *Peer) SetRemoteDescription(desc webrtc.SessionDescription) error {
	if err := p.pc.SetRemoteDescription(desc); err != nil {
		return fmt.Errorf("rtc: set remote description: %w", err)
	}
	return nil
}

// Close tears down the PeerConnection and closes the owned channels exactly once.
func (p *Peer) Close() error {
	var err error
	p.closeOnce.Do(func() {
		err = p.pc.Close()
		// Mark closed under the lock so in-flight callbacks observe it and stop
		// sending, then close the owned channels exactly once.
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		close(p.commands)
		close(p.connState)
	})
	return err
}

// discard is an io.Writer that drops everything, used for the nil-logger default.
type discard struct{}

func (discard) Write(b []byte) (int, error) { return len(b), nil }
