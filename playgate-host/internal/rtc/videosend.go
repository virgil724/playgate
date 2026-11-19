package rtc

import (
	"log/slog"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/playgate/playgate-host/internal/core"
)

// Video transport modes (config webrtc.video_transport and the per-viewer
// "active" value in the control-channel video_transport message).
const (
	VideoTransportTrack     = "track"
	VideoTransportWebCodecs = "webcodecs"
	VideoTransportAuto      = "auto"
)

// Defaults for the DataChannel sender (docs/webcodecs-transport.md section 3.4).
const (
	DefaultVideoBufferMaxBytes = 256 * 1024
	DefaultVideoIDRPaceMS      = 8
	// paceBurstFragments is the burst size when a large frame is spread out.
	paceBurstFragments = 8
	// paceThresholdFragments: frames with at most this many fragments are sent
	// back to back with no deliberate delay.
	paceThresholdFragments = 8
)

// processStart anchors send_ms (host monotonic milliseconds).
var processStart = time.Now()

// dcSender is the subset of *webrtc.DataChannel the sender needs; a fake in
// tests lets the drop policy be exercised deterministically.
type dcSender interface {
	Send(data []byte) error
	BufferedAmount() uint64
	ReadyState() webrtc.DataChannelState
}

// videoSenderConfig configures a videoSender. Zero values select defaults.
type videoSenderConfig struct {
	Mode          string // track | webcodecs | auto
	FragmentBytes int    // total message size
	BufferMax     uint64 // BufferedAmount threshold in bytes
	IDRPaceMS     int    // <0 disables pacing; 0 uses default
	Log           *slog.Logger
	// WantKeyframe asks the encoder for an IDR (coalesced/rate limited by the
	// caller). May be nil.
	WantKeyframe func()
	// FEC is the parity scheme to use: FECNone (default), FECXOR or FECRS.
	// Parity is only sent to viewers whose HELLO advertises the scheme.
	FEC int
	// FECRatio is parity fragments per data fragment (m = ceil(k*ratio)).
	FECRatio float64
	// SendAck delivers a control-channel JSON message. May be nil.
	SendAck func([]byte)
	// OnSwitchToTrack runs under the sender lock just before the active sink
	// flips to the RTP track, so the peer can re-arm its keyframe gate before
	// any frame is routed there. It must not call back into the sender. May be
	// nil.
	OnSwitchToTrack func()
}

// videoSender implements the host side of the WebCodecs transport for one
// viewer: HELLO gating, per-frame BufferedAmount drop, IDR-only-until-next-IDR,
// SPS/PPS guarantee, epoch tracking and fragmentation.
type videoSender struct {
	cfg    videoSenderConfig
	log    *slog.Logger
	stride int

	sendMu sync.Mutex // serialises whole frames onto the channel

	mu          sync.Mutex // guards the fields below
	dc          dcSender
	active      string
	hello       bool
	helloCaps   uint8
	helloMaxFEC uint8
	needsKey    bool
	linkDown    bool // peer connection not connected (see setLinkUp)
	bumpPending bool // epoch++ at the next IDR sent
	epoch       uint16
	frameID     uint32
	lastIDREp   uint16
	haveIDR     bool
	prevPTS     time.Duration
	havePTS     bool
	cachedPS    []byte
	warnedNoPS  bool
	report      FrameReport
	haveReport  bool

	framesSent    uint64
	framesDropped uint64

	// link accumulates FRAME_REPORT counters (each report covers only the
	// interval since the previous one, every ~250ms) until the ABR loop takes a
	// sample, so no report is lost between ABR samples.
	link        videoLinkSample
	dropsAtTake uint64
	// sent keeps the fragments of recently sent frames for FRAG_NACK
	// retransmission, oldest first.
	sent []sentFrame
	// retransmits counts fragments resent on FRAG_NACK (diagnostics).
	retransmits uint64

	// rttWin holds recent SCTP SRTT samples; its minimum is the uncongested
	// RTT baseline for queueing-delay detection (see linkStats).
	rttWin []time.Duration
}

// sentFrame is one frame's wire messages kept for retransmission.
type sentFrame struct {
	epoch   uint16
	frameID uint32
	at      time.Time
	msgs    [][]byte
	resent  []uint8 // per message: times resent
}

const (
	// retransmitKeep bounds how long and how many frames are kept for
	// FRAG_NACK. A retransmission older than this would arrive after the viewer
	// gave up on the frame anyway.
	retransmitKeep      = 500 * time.Millisecond
	retransmitMaxFrames = 128
	// retransmitMaxTimes caps resends of one fragment.
	retransmitMaxTimes = 2
)

// videoLinkSample is what the viewer reported about the WebCodecs path since
// the previous takeLinkSample, plus the host's own view of its send queue.
type videoLinkSample struct {
	FramesComplete uint64
	FramesLost     uint64
	FragsRecv      uint64
	FragsExpected  uint64
	// Jitter is the viewer's latest smoothed inter-frame arrival jitter.
	Jitter time.Duration
	// Reports is how many FRAME_REPORTs were folded in (0 = no data).
	Reports int
	// SenderDropped counts whole frames the host dropped because the channel's
	// send buffer was over its limit.
	SenderDropped uint64
	// Buffered is the channel's current BufferedAmount in bytes.
	Buffered uint64
}

// takeLinkSample returns the accumulated link counters and resets them.
func (s *videoSender) takeLinkSample() videoLinkSample {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.link
	s.link = videoLinkSample{}
	out.SenderDropped = s.framesDropped - s.dropsAtTake
	s.dropsAtTake = s.framesDropped
	if s.dc != nil {
		out.Buffered = s.dc.BufferedAmount()
	}
	if out.SenderDropped > 0 {
		s.log.Info("video sender dropped frames (channel backlog)",
			"frames", out.SenderDropped, "buffered", out.Buffered)
	}
	return out
}

func newVideoSender(cfg videoSenderConfig) *videoSender {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	if cfg.FragmentBytes < MinVideoFragmentBytes || cfg.FragmentBytes > MaxVideoFragmentBytes {
		cfg.FragmentBytes = DefaultVideoFragmentBytes
	}
	if cfg.BufferMax == 0 {
		cfg.BufferMax = DefaultVideoBufferMaxBytes
	}
	if cfg.IDRPaceMS == 0 {
		cfg.IDRPaceMS = DefaultVideoIDRPaceMS
	}
	s := &videoSender{
		cfg:      cfg,
		log:      cfg.Log,
		stride:   cfg.FragmentBytes - VideoHeaderSize,
		needsKey: true,
		active:   VideoTransportTrack,
	}
	if cfg.Mode == VideoTransportWebCodecs {
		s.active = VideoTransportWebCodecs
	}
	return s
}

func (s *videoSender) setChannel(dc dcSender) {
	s.mu.Lock()
	s.dc = dc
	s.needsKey = true
	s.mu.Unlock()
}

// setLinkUp tracks the peer connection state. While it is not connected the
// DataChannel can still report open, so frames are held back here; the viewer
// resumes on the next IDR and stale frames are not kept for retransmission.
func (s *videoSender) setLinkUp(up bool) {
	s.mu.Lock()
	s.linkDown = !up
	if !up {
		s.needsKey = true
		s.sent = s.sent[:0]
	}
	s.mu.Unlock()
}

// onChannelOpen marks the viewer as needing a keyframe (section 3.4).
func (s *videoSender) onChannelOpen() {
	s.mu.Lock()
	s.needsKey = true
	s.mu.Unlock()
}

// Active returns the viewer's current sink: "track" or "webcodecs".
func (s *videoSender) Active() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

func (s *videoSender) isWebCodecs() bool { return s.Active() == VideoTransportWebCodecs }

func (s *videoSender) wantKey() {
	if s.cfg.WantKeyframe != nil {
		s.cfg.WantKeyframe()
	}
}

func (s *videoSender) ack(active string) {
	if s.cfg.SendAck == nil {
		return
	}
	s.cfg.SendAck([]byte(`{"kind":"video_transport","active":"` + active + `"}`))
}

// switchTo handles a viewer's {"kind":"video_transport","use":...} request and
// returns the resulting active transport. It acknowledges, forces a keyframe
// and schedules an epoch bump. toTrack reports a switch to the RTP track so the
// caller can re-arm its keyframe gate.
func (s *videoSender) switchTo(use string) (active string, toTrack bool) {
	s.mu.Lock()
	allowed := s.cfg.Mode == VideoTransportWebCodecs || s.cfg.Mode == VideoTransportAuto
	target := s.active
	if allowed && (use == VideoTransportTrack || use == VideoTransportWebCodecs) {
		target = use
	}
	if target == VideoTransportWebCodecs && s.hello && s.helloCaps&CapWebCodecsH264 == 0 {
		target = s.active // the viewer said it cannot decode H.264 there
	}
	changed := target != s.active
	if changed {
		if target == VideoTransportTrack {
			if s.cfg.OnSwitchToTrack != nil {
				s.cfg.OnSwitchToTrack()
			}
			s.sent = s.sent[:0] // nothing left to retransmit on the channel
		}
		s.active = target
		s.needsKey = true
		s.bumpPending = true
	}
	hello := s.hello
	s.mu.Unlock()

	s.ack(target)
	if changed && (target == VideoTransportTrack || hello) {
		s.wantKey()
	}
	return target, changed && target == VideoTransportTrack
}

// handleFeedback processes one videofb message.
func (s *videoSender) handleFeedback(b []byte) {
	msg, err := ParseFeedback(b)
	if err != nil {
		s.log.Debug("bad videofb message", "err", err)
		return
	}
	switch m := msg.(type) {
	case KeyframeRequest:
		s.mu.Lock()
		stale := s.haveIDR && epochOlder(m.Epoch, s.lastIDREp)
		if !stale {
			s.needsKey = true
		}
		s.mu.Unlock()
		if stale {
			return // a newer IDR already fixed this
		}
		s.log.Debug("viewer keyframe request", "reason", m.Reason, "lost", m.LostID)
		s.wantKey()
	case FrameReport:
		s.mu.Lock()
		s.report, s.haveReport = m, true
		s.link.FramesComplete += uint64(m.FramesComplete)
		s.link.FramesLost += uint64(m.FramesLost)
		s.link.FragsRecv += uint64(m.FragsRecv)
		s.link.FragsExpected += uint64(m.FragsExpected)
		s.link.Jitter = time.Duration(m.JitterMSx10) * time.Millisecond / 10
		s.link.Reports++
		s.mu.Unlock()
	case FragNack:
		s.retransmit(m)
	case Hello:
		s.mu.Lock()
		s.hello = true
		s.helloCaps, s.helloMaxFEC = m.Caps, m.MaxFEC
		fallback := m.Caps&CapWebCodecsH264 == 0 && (s.cfg.Mode == VideoTransportWebCodecs || s.cfg.Mode == VideoTransportAuto)
		active := s.active
		if active == VideoTransportWebCodecs {
			s.needsKey = true
		}
		s.mu.Unlock()
		if fallback && active == VideoTransportWebCodecs {
			s.log.Info("viewer lacks WebCodecs H.264; falling back to track")
			s.switchTo(VideoTransportTrack)
			return
		}
		if active == VideoTransportWebCodecs {
			s.wantKey() // initial join
		}
	}
}

// fecForViewerLocked returns the configured FEC scheme if this viewer's HELLO
// advertises it, plus the viewer's max parity count. Caller holds s.mu.
func (s *videoSender) fecForViewerLocked() (scheme, maxParity int) {
	switch s.cfg.FEC {
	case FECXOR:
		if s.helloCaps&CapFECXOR == 0 {
			return FECNone, 0
		}
	case FECRS:
		if s.helloCaps&CapFECRS == 0 {
			return FECNone, 0
		}
	default:
		return FECNone, 0
	}
	return s.cfg.FEC, int(s.helloMaxFEC)
}

// lastReport returns the most recent FRAME_REPORT.
func (s *videoSender) lastReport() (FrameReport, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.report, s.haveReport
}

// stats returns sent/dropped frame counters.
func (s *videoSender) stats() (sent, dropped uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.framesSent, s.framesDropped
}

// writeFrame sends one access unit if the policy allows. It never blocks on a
// full channel; it drops the whole frame instead.
func (s *videoSender) writeFrame(pkt core.EncodedPacket) error {
	if len(pkt.Data) == 0 {
		return nil
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	s.mu.Lock()
	if !s.sendableLocked() {
		s.mu.Unlock()
		return nil
	}
	// A PTS regression means the encoder restarted (ForceKeyframe/SetBitrate):
	// new epoch at the next IDR, and nothing but an IDR until then.
	if s.havePTS && pkt.PTS < s.prevPTS {
		s.bumpPending = true
		s.needsKey = true
	}
	s.prevPTS, s.havePTS = pkt.PTS, true

	if s.needsKey && !pkt.IsKeyframe {
		s.mu.Unlock()
		return nil
	}
	if s.dc.BufferedAmount() > s.cfg.BufferMax {
		s.needsKey = true
		s.framesDropped++
		s.mu.Unlock()
		s.wantKey()
		return nil
	}

	au := pkt.Data
	flags := uint8(0)
	var prevIDREp uint16
	var prevHaveIDR bool
	if pkt.IsKeyframe {
		flags |= FlagKey
		ps, hasSPS, hasPPS := paramSets(au)
		switch {
		case hasSPS && hasPPS:
			if len(s.cachedPS) > 0 && string(s.cachedPS) != string(ps) {
				s.bumpPending = true // stream parameters changed
			}
			s.cachedPS = append(s.cachedPS[:0], ps...)
			flags |= FlagParam
		case len(s.cachedPS) > 0:
			// Encoder omitted in-band parameter sets: prepend the cached ones.
			au = append(append(make([]byte, 0, len(s.cachedPS)+len(au)), s.cachedPS...), au...)
			flags |= FlagParam
		default:
			if !s.warnedNoPS {
				s.warnedNoPS = true
				s.log.Warn("keyframe has no in-band SPS/PPS and none cached; viewer may not decode")
			}
		}
		if s.bumpPending {
			s.epoch++
			s.bumpPending = false
		}
		// Cleared before sending so a KEYFRAME_REQUEST racing with this IDR's
		// send re-arms needsKey instead of being overwritten afterwards.
		s.needsKey = false
		prevIDREp, prevHaveIDR = s.lastIDREp, s.haveIDR
		s.lastIDREp = s.epoch
		s.haveIDR = true
	}
	s.frameID++
	meta := FrameMeta{
		Flags:   flags,
		Epoch:   s.epoch,
		FrameID: s.frameID,
		PTSUs:   uint32(pkt.PTS.Microseconds()),
		SendMS:  uint32(time.Since(processStart).Milliseconds()),
	}
	dc := s.dc
	fecScheme, fecMax := s.fecForViewerLocked()
	s.mu.Unlock()

	scheme, m := FECNone, 0
	if fecScheme != FECNone {
		if m = FECParityCount(fecScheme, FragmentCount(len(au), s.stride), s.cfg.FECRatio, fecMax); m > 0 {
			scheme = fecScheme
		}
	}
	// An IDR that did not go out must not make the viewer's keyframe requests
	// for the previous epoch look stale.
	unsent := func() {
		s.mu.Lock()
		s.needsKey = true
		s.framesDropped++
		if pkt.IsKeyframe && s.lastIDREp == meta.Epoch {
			s.lastIDREp, s.haveIDR = prevIDREp, prevHaveIDR
		}
		s.mu.Unlock()
	}
	msgs, err := FragmentFrameFEC(meta, au, s.stride, scheme, m)
	if err != nil {
		unsent()
		s.log.Warn("cannot fragment frame", "err", err)
		s.wantKey()
		return nil
	}

	gap := time.Duration(0)
	if len(msgs) > paceThresholdFragments && s.cfg.IDRPaceMS > 0 {
		bursts := (len(msgs) + paceBurstFragments - 1) / paceBurstFragments
		gap = time.Duration(s.cfg.IDRPaceMS) * time.Millisecond / time.Duration(bursts)
	}
	for i, m := range msgs {
		if gap > 0 && i > 0 && i%paceBurstFragments == 0 {
			time.Sleep(gap)
		}
		if err := dc.Send(m); err != nil {
			unsent()
			s.log.Debug("video fragment send failed", "err", err)
			s.wantKey()
			return nil
		}
	}

	s.mu.Lock()
	s.framesSent++
	s.rememberLocked(sentFrame{epoch: meta.Epoch, frameID: meta.FrameID, at: time.Now(), msgs: msgs, resent: make([]uint8, len(msgs))})
	s.mu.Unlock()
	return nil
}

// rememberLocked stores f for retransmission and trims old frames. Caller
// holds s.mu.
func (s *videoSender) rememberLocked(f sentFrame) {
	s.sent = append(s.sent, f)
	cut := 0
	for cut < len(s.sent) && (len(s.sent)-cut > retransmitMaxFrames || f.at.Sub(s.sent[cut].at) > retransmitKeep) {
		cut++
	}
	if cut > 0 {
		s.sent = append(s.sent[:0], s.sent[cut:]...)
	}
}

// sendableLocked reports whether frames may go out on the channel now.
// Caller holds s.mu.
func (s *videoSender) sendableLocked() bool {
	return s.active == VideoTransportWebCodecs && s.hello && !s.linkDown && s.dc != nil &&
		s.dc.ReadyState() == webrtc.DataChannelStateOpen
}

// retransmitMaxFragments caps the fragments one FRAG_NACK can pull back out.
// An empty index list asks for the whole frame, which for an IDR can be
// hundreds of fragments; past this many the viewer is better served by the
// keyframe request it falls back to.
const retransmitMaxFragments = 64

// retransmit resends the fragments a FRAG_NACK asks for, if the frame is
// still kept and the channel is not backed up. Each index is sent at most once
// per request, at most retransmitMaxTimes per fragment and retransmitMaxFragments
// per request, and sending stops as soon as the channel backs up.
func (s *videoSender) retransmit(n FragNack) {
	s.mu.Lock()
	dc := s.dc
	if !s.sendableLocked() || dc.BufferedAmount() > s.cfg.BufferMax {
		s.mu.Unlock()
		return
	}
	var out [][]byte
	for i := range s.sent {
		f := &s.sent[i]
		if f.epoch != n.Epoch || f.frameID != n.FrameID {
			continue
		}
		seen := make(map[int]bool, len(n.Indices))
		want := func(j int) {
			if len(out) >= retransmitMaxFragments || seen[j] {
				return
			}
			seen[j] = true
			if j < len(f.msgs) && f.resent[j] < retransmitMaxTimes {
				f.resent[j]++
				out = append(out, f.msgs[j])
			}
		}
		if len(n.Indices) == 0 {
			for j := range f.msgs {
				want(j)
			}
		} else {
			for _, j := range n.Indices {
				want(int(j))
			}
		}
		break
	}
	s.mu.Unlock()
	sent := 0
	for _, m := range out {
		if dc.BufferedAmount() > s.cfg.BufferMax {
			break
		}
		if err := dc.Send(m); err != nil {
			s.log.Debug("video retransmit failed", "err", err)
			break
		}
		sent++
	}
	s.mu.Lock()
	s.retransmits += uint64(sent)
	s.mu.Unlock()
}
