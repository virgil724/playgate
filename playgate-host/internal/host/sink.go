package host

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/playgate/playgate-host/internal/core"
	"github.com/playgate/playgate-host/internal/session"
)

// inputSink connects a viewer's decoded command stream to the input target and,
// when session gating is enabled, enforces JWT-validated single-controller
// access via the session manager and forwards session events to the viewer's
// control channel.
//
// The connection manager creates one sink interaction per viewer connection:
//   - session disabled: call Passthrough to stream commands straight to the
//     target.
//   - session enabled: register OnControlMessage to capture the viewer's auth
//     token, then call Authorize to obtain a gated channel and drain it.
type inputSink struct {
	log     *slog.Logger
	target  core.InputTarget
	manager *session.Manager // nil when gating is disabled
}

// newInputSink builds the host's InputSink from its deps and session config.
func (h *Host) newInputSink() *inputSink {
	return &inputSink{
		log:     h.log.With("module", "input-sink"),
		target:  h.deps.Input,
		manager: h.session,
	}
}

// SessionEnabled reports whether JWT gating is active.
func (s *inputSink) SessionEnabled() bool { return s.manager != nil }

// authMessage is the control-channel frame a viewer sends to present its session
// JWT: {"kind":"auth","token":"<jwt>"}.
type authMessage struct {
	Kind  string `json:"kind"`
	Token string `json:"token"`
}

// HandleCommands implements InputSink for the session-disabled case (the common
// dev path). It streams every command straight to the target. The gated path is
// driven directly by the connection manager via Authorize.
func (s *inputSink) HandleCommands(ctx context.Context, raw <-chan core.InputCommand, sendControl func([]byte) error) {
	if s.manager == nil {
		s.drainToTarget(ctx, raw)
		return
	}
	// Gated path: forward session events for the connection lifetime. Command
	// gating itself is wired by the connection manager once a token arrives.
	s.forwardEvents(ctx, sendControl)
}

// drainToTarget forwards every command on raw to the input target until raw
// closes or ctx is cancelled. On ANY exit — channel close (session expiry/kick)
// OR ctx cancel (viewer disconnect / shutdown) — it sends a neutral (all-released)
// command so a button held at the moment control is lost does not stick down on
// the Switch. The defer covers the ctx-cancel race that a close-only reset misses.
func (s *inputSink) drainToTarget(ctx context.Context, raw <-chan core.InputCommand) {
	defer func() {
		if err := s.target.Send(core.InputCommand{}); err != nil {
			s.log.Debug("neutral reset send failed", "err", err)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case cmd, ok := <-raw:
			if !ok {
				return
			}
			if err := s.target.Send(cmd); err != nil {
				s.log.Debug("input send failed", "err", err)
			}
		}
	}
}

// Authorize validates token, claims control for the viewer, and streams the
// gated commands to the target until the session ends, raw closes, or ctx is
// cancelled. It returns the claim error if the token is rejected.
func (s *inputSink) Authorize(ctx context.Context, token string, raw <-chan core.InputCommand) error {
	sess, err := s.manager.Claim(token)
	if err != nil {
		return err
	}
	s.log.Info("viewer authorized", "viewer", sess.ViewerID())
	gated := s.manager.Gate(sess.ViewerID(), raw)
	s.drainToTarget(ctx, gated)
	// drainToTarget returned: either the session ended (gate closed) or the
	// viewer's stream/ctx ended (disconnect). Free the controller slot now so a
	// queued viewer is promoted immediately instead of waiting for this viewer's
	// JWT to expire. No-op if the session already ended on its own.
	s.manager.Release(sess)
	return nil
}

// forwardEvents pushes session lifecycle events to the viewer's control channel
// as JSON (docs/protocols.md §4) until ctx is cancelled or the events channel
// closes.
func (s *inputSink) forwardEvents(ctx context.Context, sendControl func([]byte) error) {
	if sendControl == nil {
		return
	}
	events := s.manager.Events()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if err := sendControl(data); err != nil {
				s.log.Debug("control event send failed", "err", err)
			}
		}
	}
}
