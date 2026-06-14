package host

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/playgate/playgate-host/internal/config"
	"github.com/playgate/playgate-host/internal/metrics"
	"github.com/playgate/playgate-host/internal/rtc"
	"github.com/playgate/playgate-host/internal/signaling"
)

// makeSignalingConnect returns a ConnectFunc that drives the WebRTC lifecycle
// against the signaling Worker: build ICE config (TURN or STUN), then read the
// merged viewer feed and run one rtc.Peer per viewer concurrently (mesh
// broadcast). Each viewer announces itself with a "hello"; the manager replies
// with a per-viewer offer (addressed via the payload's "to"), wires the peer to
// the routers and input sink, and tears it down on that viewer's disconnect.
func makeSignalingConnect(log *slog.Logger, cfg config.Config, mc *metrics.Collector, enc BitrateController) ConnectFunc {
	return func(ctx context.Context, router *VideoRouter, arouter *AudioRouter, sink InputSink) error {
		client, err := signaling.New(signaling.Config{
			BaseURL: cfg.Signaling.URL,
			RoomID:  cfg.Signaling.RoomID,
			Token:   cfg.Signaling.Token,
			Logger:  log,
		})
		if err != nil {
			return err
		}

		ice := resolveICEServers(ctx, log, cfg, client)
		poll := time.Duration(cfg.Signaling.PollIntervalMS) * time.Millisecond
		if poll <= 0 {
			poll = 500 * time.Millisecond
		}

		cm := &connManager{
			log:    log.With("module", "conn-manager"),
			cfg:    cfg,
			mc:     mc,
			enc:    enc,
			client:  client,
			router:  router,
			arouter: arouter,
			sink:    sink.(*inputSink),
			ice:     ice,
			poll:    poll,
		}
		return cm.loop(ctx)
	}
}

// resolveICEServers fetches TURN credentials when enabled, falling back to the
// static STUN list on any error.
func resolveICEServers(ctx context.Context, log *slog.Logger, cfg config.Config, client *signaling.Client) []webrtc.ICEServer {
	static := rtc.ICEServersFromURLs(cfg.WebRTC.ICEServers)
	if !cfg.Signaling.UseTURN {
		return static
	}
	creds, err := client.TURNCredentials(ctx)
	if err != nil {
		log.Warn("turn credentials failed; falling back to STUN", "err", err)
		return static
	}
	var out []webrtc.ICEServer
	for _, s := range creds {
		urls := s.URLList()
		if len(urls) == 0 {
			continue
		}
		out = append(out, webrtc.ICEServer{
			URLs:       urls,
			Username:   s.Username,
			Credential: s.Credential,
		})
	}
	if len(out) == 0 {
		return static
	}
	log.Info("using TURN ice servers", "count", len(out))
	return out
}

// connManager reads the merged viewer feed and runs one peer per viewer.
type connManager struct {
	log     *slog.Logger
	cfg     config.Config
	mc      *metrics.Collector
	enc     BitrateController // ABR target; unused in multi-viewer (see runViewer)
	client  *signaling.Client
	router  *VideoRouter
	arouter *AudioRouter // nil when audio is disabled
	sink    *inputSink
	ice     []webrtc.ICEServer
	poll    time.Duration

	mu       sync.Mutex
	sessions map[string]*viewerSession // keyed by viewerId
}

// viewerSession is one connected viewer's peer plus its answer-applied guard.
type viewerSession struct {
	mu       sync.Mutex
	peer     *rtc.Peer
	answered bool
}

func (s *viewerSession) setPeer(p *rtc.Peer) {
	s.mu.Lock()
	s.peer = p
	s.mu.Unlock()
}

// viewerEnvelope is the JSON shape of a viewer→host signaling payload. viewerId
// addresses which viewer's session a message belongs to; kind=="hello" announces
// a new viewer so the host knows to send it an offer.
type viewerEnvelope struct {
	Kind     string `json:"kind"`
	Type     string `json:"type"`
	SDP      string `json:"sdp"`
	ViewerID string `json:"viewerId"`
}

// viewerOffer is the host→viewer offer payload; To carries the target viewerId
// so the Worker resets only that viewer's session and the browser can filter.
type viewerOffer struct {
	Type string `json:"type"`
	SDP  string `json:"sdp"`
	To   string `json:"to"`
}

// loop reads the merged viewer feed for the room's lifetime, reconnecting on
// transport failure until ctx is cancelled.
func (c *connManager) loop(ctx context.Context) error {
	c.mu.Lock()
	c.sessions = make(map[string]*viewerSession)
	c.mu.Unlock()
	for {
		if ctx.Err() != nil {
			return nil
		}
		if err := c.runFeed(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("signaling feed ended; retrying", "err", err)
			if !sleepCtx(ctx, c.poll) {
				return nil
			}
		}
	}
}

// runFeed reads the merged viewer→host feed (every viewer's hello/answer, each
// tagged with viewerId) and dispatches each message. It prefers a WebSocket and
// falls back to HTTP long-poll. Returns when the transport fails (caller retries)
// or ctx is cancelled.
func (c *connManager) runFeed(ctx context.Context) error {
	if c.runFeedWS(ctx) {
		return nil // ctx cancelled
	}
	if ctx.Err() != nil {
		return nil
	}
	return c.runFeedLongPoll(ctx)
}

// runFeedWS reads the feed over a WebSocket. Returns true only when ctx was
// cancelled (clean stop); a dial/receive failure returns false so runFeed falls
// back to long-poll. The Worker replays the viewer backlog on connect, so
// reconnects re-dispatch already-handled messages — handleFeedMessage is
// idempotent (existing sessions ignore repeat hellos/answers).
func (c *connManager) runFeedWS(ctx context.Context) bool {
	conn, err := c.client.DialWS(ctx, signaling.PeerHost)
	if err != nil {
		c.log.Debug("ws dial failed; falling back to long-poll", "err", err)
		return false
	}
	defer conn.Close()
	c.log.Info("signaling feed connected", "transport", "websocket")
	for {
		m, err := conn.Receive(ctx)
		if err != nil {
			return ctx.Err() != nil
		}
		c.handleFeedMessage(ctx, m)
	}
}

// runFeedLongPoll is the HTTP long-poll fallback for the feed.
func (c *connManager) runFeedLongPoll(ctx context.Context) error {
	since := -1
	for {
		if ctx.Err() != nil {
			return nil
		}
		start := time.Now()
		msgs, next, err := c.client.PollWait(ctx, signaling.PeerHost, since, longPollWait)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		since = next
		for _, m := range msgs {
			c.handleFeedMessage(ctx, m)
		}
		if len(msgs) == 0 && time.Since(start) < time.Second {
			// Worker ignored the wait param (old server): throttle to poll cadence.
			if !sleepCtx(ctx, c.poll) {
				return nil
			}
		}
	}
}

// handleFeedMessage routes one viewer message to its session, starting a new
// session when an unknown viewer says hello.
func (c *connManager) handleFeedMessage(ctx context.Context, m signaling.Message) {
	var env viewerEnvelope
	if err := json.Unmarshal(m.Payload, &env); err != nil || env.ViewerID == "" {
		return
	}
	c.mu.Lock()
	sess, ok := c.sessions[env.ViewerID]
	if !ok {
		// Only a hello starts a session; stray answers for unknown viewers (e.g.
		// from a dead session) are ignored.
		if env.Kind != "hello" {
			c.mu.Unlock()
			return
		}
		sess = &viewerSession{}
		c.sessions[env.ViewerID] = sess
		c.mu.Unlock()
		go c.runViewer(ctx, env.ViewerID, sess)
		return
	}
	c.mu.Unlock()
	c.applyAnswer(sess, env)
}

// applyAnswer sets the viewer's SDP answer on its peer exactly once. ICE
// candidates are ignored (the host gathers non-trickle, so candidates are in the
// offer/answer SDP).
func (c *connManager) applyAnswer(sess *viewerSession, env viewerEnvelope) {
	if env.Type != "answer" || env.SDP == "" {
		return
	}
	sess.mu.Lock()
	peer := sess.peer
	if peer == nil || sess.answered {
		sess.mu.Unlock()
		return
	}
	sess.answered = true
	sess.mu.Unlock()
	if err := peer.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  env.SDP,
	}); err != nil {
		c.log.Warn("set remote answer failed", "err", err)
	}
}

// runViewer builds the peer for one viewer, registers it with the routers, sends
// the offer, and waits for that viewer to disconnect, then cleans up.
//
// NOTE: we intentionally do NOT wire OnKeyframeRequest to the encoder — an
// on-demand IDR means restarting ffmpeg (new SPS/PPS, PTS reset), which disrupts
// every other viewer. A late joiner instead waits for the next scheduled GOP
// keyframe (per-peer gating in rtc.Peer.WriteSample).
//
// ABR is intentionally not run per viewer: it drives the single shared encoder
// bitrate, so N viewers would fight over it. Multi-viewer runs at fixed bitrate.
func (c *connManager) runViewer(ctx context.Context, id string, sess *viewerSession) {
	log := c.log.With("viewer", id)
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer func() {
		c.mu.Lock()
		delete(c.sessions, id)
		c.mu.Unlock()
	}()

	peer, err := rtc.NewPeer(rtc.Config{ICEServers: c.ice, Logger: log, EnableAudio: c.arouter != nil})
	if err != nil {
		log.Warn("create peer failed", "err", err)
		return
	}
	defer peer.Close()
	sess.setPeer(peer)

	c.router.AddSink(peer)
	defer c.router.RemoveSink(peer)
	if c.arouter != nil {
		c.arouter.AddSink(peer)
		defer c.arouter.RemoveSink(peer)
	}

	c.wireInput(sctx, peer)

	offer, err := peer.CreateOffer(sctx)
	if err != nil {
		log.Warn("create offer failed", "err", err)
		return
	}
	if err := c.client.PostMessage(sctx, viewerOffer{
		Type: offer.Type.String(),
		SDP:  offer.SDP,
		To:   id,
	}); err != nil {
		log.Warn("post offer failed", "err", err)
		return
	}
	log.Info("offer posted to viewer; awaiting answer")

	c.awaitDisconnect(sctx, peer)
	log.Info("viewer disconnected")
}

// wireInput connects the peer's command stream to the input sink. For the gated
// path it registers a control-channel handler that captures the viewer's auth
// token and starts authorization.
func (c *connManager) wireInput(ctx context.Context, peer *rtc.Peer) {
	sendControl := func(b []byte) error { return peer.SendControl(b) }

	if !c.sink.SessionEnabled() {
		go c.sink.HandleCommands(ctx, peer.Commands(), sendControl)
		return
	}

	// Gated: forward session events for the connection lifetime.
	go c.sink.HandleCommands(ctx, peer.Commands(), sendControl)

	// Authorize on each auth token. A viewer may redeem more than one token over a
	// single connection (play a session, let it end, redeem again), so we re-arm
	// after each authorization finishes rather than accepting only the first.
	// Only one authorization runs at a time: Authorize drains the single Commands
	// stream, so a second concurrent one would split it. While one is in flight,
	// further tokens are ignored; the next is honoured once it ends.
	var authMu sync.Mutex
	authorizing := false
	peer.OnControlMessage(func(data []byte) {
		var msg authMessage
		if err := json.Unmarshal(data, &msg); err != nil || msg.Kind != "auth" || msg.Token == "" {
			return
		}
		authMu.Lock()
		if authorizing {
			authMu.Unlock()
			return
		}
		authorizing = true
		authMu.Unlock()
		go func() {
			if err := c.sink.Authorize(ctx, msg.Token, peer.Commands()); err != nil {
				c.log.Warn("viewer authorization failed", "err", err)
			}
			authMu.Lock()
			authorizing = false
			authMu.Unlock()
		}()
	})
}

// longPollWait is the duration requested from the Durable Object Worker.
// The server caps its own wait at 25s; we ask for exactly 25s.
const longPollWait = 25 * time.Second

// awaitDisconnect blocks until the peer reaches a terminal connection state or
// ctx is cancelled.
func (c *connManager) awaitDisconnect(ctx context.Context, peer *rtc.Peer) {
	for {
		select {
		case <-ctx.Done():
			return
		case s, ok := <-peer.ConnState():
			if !ok {
				return
			}
			switch s {
			case webrtc.PeerConnectionStateFailed,
				webrtc.PeerConnectionStateClosed,
				webrtc.PeerConnectionStateDisconnected:
				return
			}
		}
	}
}

// sleepCtx sleeps d or returns false if ctx is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
