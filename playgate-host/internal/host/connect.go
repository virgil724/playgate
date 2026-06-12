package host

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/playgate/playgate-host/internal/config"
	"github.com/playgate/playgate-host/internal/metrics"
	"github.com/playgate/playgate-host/internal/rtc"
	"github.com/playgate/playgate-host/internal/signaling"
)

// errTimeoutNoAnswer is returned when no viewer answer arrives within the
// per-connection deadline, so the manager recycles the peer and tries again.
var errTimeoutNoAnswer = errors.New("host: timed out waiting for viewer answer")

// makeSignalingConnect returns a ConnectFunc that drives the per-viewer WebRTC
// lifecycle against the signaling Worker: build ICE config (TURN or STUN), then
// loop accepting one viewer at a time. Each iteration creates a fresh rtc.Peer,
// posts an offer, polls for the viewer's answer + ICE candidates, wires the peer
// to the router and input sink, and tears down on disconnect so the next viewer
// can connect.
func makeSignalingConnect(log *slog.Logger, cfg config.Config, mc *metrics.Collector, enc BitrateController) ConnectFunc {
	return func(ctx context.Context, router *VideoRouter, sink InputSink) error {
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
			client: client,
			router: router,
			sink:   sink.(*inputSink),
			ice:    ice,
			poll:   poll,
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

// connManager runs the accept-one-viewer-at-a-time loop.
type connManager struct {
	log    *slog.Logger
	cfg    config.Config
	mc     *metrics.Collector
	enc    BitrateController // ABR target; nil disables adaptive bitrate
	client *signaling.Client
	router *VideoRouter
	sink   *inputSink
	ice    []webrtc.ICEServer
	poll   time.Duration
}

// loop repeatedly serves a single viewer connection until ctx is cancelled.
func (c *connManager) loop(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		if err := c.serveOne(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("viewer session ended with error; will accept next viewer", "err", err)
			// brief backoff to avoid a hot loop if signaling is down.
			if !sleepCtx(ctx, c.poll) {
				return nil
			}
		}
	}
}

// serveOne handles exactly one viewer connection from offer to disconnect.
func (c *connManager) serveOne(ctx context.Context) error {
	peer, err := rtc.NewPeer(rtc.Config{ICEServers: c.ice, Logger: c.log})
	if err != nil {
		return err
	}
	defer peer.Close()

	// Connection-scoped context so all per-viewer goroutines stop on disconnect.
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Register the peer as the active video sink and clear it on teardown.
	c.router.SetSink(peer)
	defer c.router.Clear()

	// Wire input: gated or pass-through depending on session config.
	c.wireInput(connCtx, peer)

	// --- signaling: post offer, poll for answer + ICE ---
	offer, err := peer.CreateOffer(ctx)
	if err != nil {
		return err
	}
	offerTs, err := c.client.PostOffer(ctx, signaling.SDPMessage{Type: offer.Type.String(), SDP: offer.SDP})
	if err != nil {
		return err
	}
	c.log.Info("offer posted; waiting for viewer answer", "offer_ts", offerTs)

	if err := c.pollForAnswer(connCtx, peer, offerTs); err != nil {
		return err
	}

	// --- T14: adaptive bitrate. Sample this peer's WebRTC stats and drive the
	// encoder bitrate for the connection lifetime. nil runner = ABR disabled. ---
	if runner := newABRRunner(c.log, c.cfg, peer, c.enc); runner != nil {
		go runner.run(connCtx)
	}

	// --- wait for the connection to terminate ---
	c.awaitDisconnect(connCtx, peer)
	c.log.Info("viewer disconnected; recycling peer")
	return nil
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

	// Capture the auth token from the control channel exactly once, then start
	// authorization which drains the gated command stream.
	authorized := make(chan struct{})
	peer.OnControlMessage(func(data []byte) {
		var msg authMessage
		if err := json.Unmarshal(data, &msg); err != nil || msg.Kind != "auth" || msg.Token == "" {
			return
		}
		select {
		case <-authorized:
			return // already authorized
		default:
		}
		close(authorized)
		go func() {
			if err := c.sink.Authorize(ctx, msg.Token, peer.Commands()); err != nil {
				c.log.Warn("viewer authorization failed", "err", err)
			}
		}()
	})
}

// longPollWait is the duration requested from the Durable Object Worker.
// The server caps its own wait at 25s; we ask for exactly 25s.
const longPollWait = 25 * time.Second

// pollForAnswer polls the Worker for the viewer's answer and ICE candidates and
// applies them, returning once the answer has been set or ctx is cancelled.
// offerTs is the Worker-assigned timestamp of our own offer; answers older than
// it are stale leftovers from previous attempts and are skipped ("" = unknown,
// apply unconditionally).
//
// Long-poll mode: each iteration calls PollWait(..., 25s) so a Durable Object
// Worker can hold the request open until a message arrives. Old Workers that
// ignore the wait param return immediately; the anti-hot-spin guard detects that
// (round-trip < 1s with no messages) and sleeps c.poll before the next call,
// degrading to the pre-existing polling behaviour.
func (c *connManager) pollForAnswer(ctx context.Context, peer *rtc.Peer, offerTs string) error {
	// Bound the wait for an answer so a stale room doesn't block forever; once an
	// answer arrives we keep polling for trickle ICE within the connection ctx.
	deadline := time.Now().Add(60 * time.Second)

	// Prefer a WebSocket: an idle WS accrues zero Durable Object duration, whereas
	// a held long-poll keeps the per-room DO awake. Dial once; on any failure fall
	// back to the long-poll loop below (old Workers / test servers don't speak WS).
	if c.receiveAnswerWS(ctx, peer, offerTs, deadline) {
		return nil
	}
	if ctx.Err() != nil {
		return nil
	}
	if !time.Now().Before(deadline) {
		return errTimeoutNoAnswer
	}

	return c.pollForAnswerLongPoll(ctx, peer, offerTs, deadline)
}

// receiveAnswerWS dials the WebSocket and, on success, feeds replayed and live
// viewer frames through applyViewerMessage until an answer is applied or the read
// fails / the deadline passes. It returns true only when an answer was applied
// (the caller is then done). A dial failure, a Receive error before any answer,
// or ctx cancellation returns false so the caller can fall back to long-poll
// (unless ctx was cancelled, which the caller re-checks).
func (c *connManager) receiveAnswerWS(ctx context.Context, peer *rtc.Peer, offerTs string, deadline time.Time) bool {
	wsCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	conn, err := c.client.DialWS(wsCtx, signaling.PeerHost)
	if err != nil {
		c.log.Debug("ws dial failed; falling back to long-poll", "err", err)
		return false
	}
	defer conn.Close()

	for {
		// Receive blocks on the socket but honours wsCtx, so a cancelled parent
		// ctx or the elapsed deadline aborts the parked read promptly.
		m, err := conn.Receive(wsCtx)
		if err != nil {
			if ctx.Err() == nil && time.Now().Before(deadline) {
				c.log.Debug("ws receive failed; falling back to long-poll", "err", err)
			}
			return false
		}
		if c.applyViewerMessage(peer, m, offerTs) {
			// applyViewerMessage already logged the apply; just note the transport.
			c.log.Info("answer transport", "transport", "websocket")
			return true
		}
	}
}

// pollForAnswerLongPoll is the original HTTP long-poll path, retained as a
// fallback for Workers that do not speak WebSocket. It runs until an answer is
// applied, the deadline passes (errTimeoutNoAnswer), or ctx is cancelled.
func (c *connManager) pollForAnswerLongPoll(ctx context.Context, peer *rtc.Peer, offerTs string, deadline time.Time) error {
	since := -1
	gotAnswer := false
	longPollLogged := false

	for {
		if ctx.Err() != nil {
			return nil
		}

		start := time.Now()
		msgs, next, err := c.client.PollWait(ctx, signaling.PeerHost, since, longPollWait)
		elapsed := time.Since(start)

		if err != nil {
			// ctx cancellation — exit cleanly.
			if ctx.Err() != nil {
				return nil
			}
			c.log.Debug("poll failed", "err", err)
			// Throttle errors so they don't hot-loop.
			if !sleepCtx(ctx, c.poll) {
				return nil
			}
			continue
		}

		since = next

		if len(msgs) > 0 {
			if !longPollLogged {
				longPollLogged = true
				c.log.Info("long-poll mode active", "first_wait", elapsed.Round(time.Millisecond))
			}
			for _, m := range msgs {
				if c.applyViewerMessage(peer, m, offerTs) {
					gotAnswer = true
				}
			}
		} else if elapsed < time.Second {
			// Server returned immediately with no messages — it does not understand
			// the wait parameter (old Worker). Log once and degrade to polling cadence.
			c.log.Debug("server ignored long-poll wait; degrading to short-poll", "elapsed", elapsed.Round(time.Millisecond))
			if !sleepCtx(ctx, c.poll) {
				return nil
			}
		}
		// If elapsed >= 1s and no messages, the server held and timed out on its
		// end — that's normal; loop immediately for the next long-poll.

		if !gotAnswer && time.Now().After(deadline) {
			return errTimeoutNoAnswer
		}
		if gotAnswer {
			// Answer applied; ICE is non-trickle on both ends so we can stop.
			return nil
		}
	}
}

// applyViewerMessage interprets one polled message as either an SDP answer or an
// ICE candidate and applies it. It returns true if an answer was applied.
// Answers timestamped before offerTs are stale (they answer a previous, dead
// peer's offer) and are skipped; both timestamps were assigned by the Worker's
// clock, so a lexicographic ISO-8601 comparison is safe — no host/Worker clock
// skew is involved. offerTs=="" (older Worker that returned no ts) disables the
// check for backward compatibility.
func (c *connManager) applyViewerMessage(peer *rtc.Peer, msg signaling.Message, offerTs string) bool {
	var sdp signaling.SDPMessage
	if err := json.Unmarshal(msg.Payload, &sdp); err == nil && sdp.Type == "answer" && sdp.SDP != "" {
		if offerTs != "" && msg.Ts != "" && msg.Ts < offerTs {
			c.log.Info("ignoring stale viewer answer", "answer_ts", msg.Ts, "offer_ts", offerTs)
			return false
		}
		if err := peer.SetRemoteDescription(webrtc.SessionDescription{
			Type: webrtc.SDPTypeAnswer,
			SDP:  sdp.SDP,
		}); err != nil {
			c.log.Warn("set remote answer failed", "err", err)
			return false
		}
		c.log.Info("viewer answer applied")
		return true
	}
	// Otherwise it may be a trickle ICE candidate; non-trickle hosts ignore these.
	return false
}

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
