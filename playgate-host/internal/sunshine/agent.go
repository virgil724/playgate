package sunshine

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/playgate/playgate-host/internal/session"
)

// AgentConfig controls the Agent's behaviour.
type AgentConfig struct {
	// Controller is the Sunshine API client.  Required.
	Controller Controller

	// Events is the channel produced by session.Manager.Events().  The Agent
	// reads from this channel until it is closed or ctx is cancelled.
	// Required.
	Events <-chan session.SessionEvent

	// PairPIN, if non-empty, is the 4-digit PIN that the Agent will submit to
	// Sunshine whenever a session is granted.  In a real deployment the PIN is
	// entered by the viewer in the Moonlight app; for automated approval this
	// field lets the operator pre-configure it.
	//
	// TODO: full integration will read the PIN out-of-band (e.g. the viewer
	// submits it to playgate-server which relays it here).
	PairPIN string

	// KickRetries is how many times the Agent retries a failed KickAll call
	// before giving up and logging a warning.  Defaults to 3.
	KickRetries int

	// KickRetryDelay is the pause between KickAll retries.  Defaults to 1s.
	KickRetryDelay time.Duration

	// Log is the structured logger.  Defaults to the default slog logger if nil.
	Log *slog.Logger
}

func (c *AgentConfig) kickRetries() int {
	if c.KickRetries > 0 {
		return c.KickRetries
	}
	return 3
}

func (c *AgentConfig) kickRetryDelay() time.Duration {
	if c.KickRetryDelay > 0 {
		return c.KickRetryDelay
	}
	return time.Second
}

func (c *AgentConfig) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

// Agent is the Sunshine management loop.
//
// # State machine
//
//	              EventGranted
//	  idle ───────────────────────▶ active
//	                                  │
//	           EventExpired           │   EventIdleKicked
//	          ┌────────────────────── ┤ ──────────────────┐
//	          ▼                                            ▼
//	       kick+idle                                   kick+idle
//
// On EventGranted the Agent optionally calls ApprovePair (if PairPIN != "")
// so that a pending Moonlight pairing is completed automatically.
//
// On EventExpired or EventIdleKicked the Agent calls KickAll to sever any
// active Moonlight connection, then returns to idle.
//
// Other event kinds (EventQueued, EventTick) are logged at debug level and
// otherwise ignored.
type Agent struct {
	cfg AgentConfig
}

// NewAgent creates an Agent with the given config.  Call Run to start the
// event loop.
func NewAgent(cfg AgentConfig) (*Agent, error) {
	if cfg.Controller == nil {
		return nil, fmt.Errorf("sunshine.NewAgent: Controller must not be nil")
	}
	if cfg.Events == nil {
		return nil, fmt.Errorf("sunshine.NewAgent: Events channel must not be nil")
	}
	return &Agent{cfg: cfg}, nil
}

// Run blocks until ctx is cancelled or the Events channel is closed.
// It is safe to call Run in a goroutine and cancel ctx to shut it down.
//
// Run implements core.Module (Name + Run) so it can be added to an errgroup.
func (a *Agent) Run(ctx context.Context) error {
	log := a.cfg.log()
	log.Info("sunshine agent started")

	for {
		select {
		case <-ctx.Done():
			log.Info("sunshine agent stopping (context cancelled)")
			return nil

		case ev, ok := <-a.cfg.Events:
			if !ok {
				log.Info("sunshine agent stopping (events channel closed)")
				return nil
			}
			a.handleEvent(ctx, ev)
		}
	}
}

// Name implements core.Module.
func (a *Agent) Name() string { return "sunshine-agent" }

// handleEvent dispatches a single SessionEvent.
func (a *Agent) handleEvent(ctx context.Context, ev session.SessionEvent) {
	log := a.cfg.log().With("event", ev.Kind, "viewer", ev.ViewerID)

	switch ev.Kind {
	case session.EventGranted:
		log.Info("session granted — viewer is now active",
			"remaining_seconds", ev.RemainingSeconds)
		if a.cfg.PairPIN != "" {
			a.approvePair(ctx, ev.ViewerID)
		} else {
			log.Debug("PairPIN not configured; skipping ApprovePair")
		}

	case session.EventExpired:
		log.Info("session expired — kicking Moonlight clients")
		a.kickWithRetry(ctx, ev.ViewerID, "session_expired")

	case session.EventIdleKicked:
		log.Info("session idle-kicked — kicking Moonlight clients")
		a.kickWithRetry(ctx, ev.ViewerID, "idle_kicked")

	case session.EventQueued:
		log.Debug("viewer queued", "queue_position", ev.QueuePosition)

	case session.EventTick:
		log.Debug("session tick", "remaining_seconds", ev.RemainingSeconds)

	default:
		log.Debug("unknown event kind (ignored)", "kind", string(ev.Kind))
	}
}

// approvePair calls Controller.ApprovePair with the configured PIN.
func (a *Agent) approvePair(ctx context.Context, viewerID string) {
	log := a.cfg.log().With("viewer", viewerID)
	if err := a.cfg.Controller.ApprovePair(ctx, a.cfg.PairPIN); err != nil {
		log.Warn("ApprovePair failed (non-fatal — viewer may still pair manually)",
			"err", err)
		return
	}
	log.Info("ApprovePair succeeded")
}

// kickWithRetry calls KickAll, retrying on failure.
func (a *Agent) kickWithRetry(ctx context.Context, viewerID, reason string) {
	log := a.cfg.log().With("viewer", viewerID, "reason", reason)
	maxRetries := a.cfg.kickRetries()
	delay := a.cfg.kickRetryDelay()

	for attempt := 1; attempt <= maxRetries; attempt++ {
		if err := a.cfg.Controller.KickAll(ctx); err != nil {
			log.Warn("KickAll failed",
				"attempt", attempt,
				"max", maxRetries,
				"err", err)
			if attempt < maxRetries {
				select {
				case <-ctx.Done():
					log.Warn("KickAll retry aborted (context cancelled)")
					return
				case <-time.After(delay):
				}
			}
			continue
		}
		log.Info("KickAll succeeded", "attempt", attempt)
		return
	}
	log.Error("KickAll failed after all retries — Moonlight client may remain connected",
		"retries", maxRetries)
}
