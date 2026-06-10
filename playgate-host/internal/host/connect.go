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
	if err := c.client.PostOffer(ctx, signaling.SDPMessage{Type: offer.Type.String(), SDP: offer.SDP}); err != nil {
		return err
	}
	c.log.Info("offer posted; waiting for viewer answer")

	if err := c.pollForAnswer(connCtx, peer); err != nil {
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

// pollForAnswer polls the Worker for the viewer's answer and ICE candidates and
// applies them, returning once the answer has been set or ctx is cancelled.
func (c *connManager) pollForAnswer(ctx context.Context, peer *rtc.Peer) error {
	since := -1
	gotAnswer := false
	ticker := time.NewTicker(c.poll)
	defer ticker.Stop()

	// Bound the wait for an answer so a stale room doesn't block forever; once an
	// answer arrives we keep polling for trickle ICE within the connection ctx.
	deadline := time.Now().Add(60 * time.Second)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			payloads, next, err := c.client.Poll(ctx, signaling.PeerHost, since)
			if err != nil {
				c.log.Debug("poll failed", "err", err)
				continue
			}
			since = next
			for _, p := range payloads {
				if c.applyViewerMessage(peer, p) {
					gotAnswer = true
				}
			}
			if !gotAnswer && time.Now().After(deadline) {
				return errTimeoutNoAnswer
			}
			if gotAnswer {
				// Answer applied; ICE is non-trickle on both ends so we can stop.
				return nil
			}
		}
	}
}

// applyViewerMessage interprets one polled payload as either an SDP answer or an
// ICE candidate and applies it. It returns true if an answer was applied.
func (c *connManager) applyViewerMessage(peer *rtc.Peer, payload json.RawMessage) bool {
	var sdp signaling.SDPMessage
	if err := json.Unmarshal(payload, &sdp); err == nil && sdp.Type == "answer" && sdp.SDP != "" {
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
