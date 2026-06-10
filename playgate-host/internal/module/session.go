package module

import (
	"context"
	"log/slog"
	"time"

	"github.com/playgate/playgate-host/internal/core"
)

// Session is a stub for the operation-control / turn-management module (T9). The
// real implementation decides which viewer currently holds control and may gate
// or rewrite commands flowing to the InputTarget. The stub is a passive
// heartbeat loop so the lifecycle is exercised.
//
// It does not own any pipeline channels; it is a control-plane module wired into
// the same errgroup so its lifetime matches the rest of the host.
type Session struct {
	log *slog.Logger
}

// NewSession constructs a stub Session module.
func NewSession(log *slog.Logger) *Session {
	return &Session{log: log.With("module", "session")}
}

// Name implements core.Module.
func (s *Session) Name() string { return "session" }

// Run implements core.Module.
func (s *Session) Run(ctx context.Context) error {
	s.log.Info("started")
	defer s.log.Info("stopped")

	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.log.Debug("session heartbeat (stub)")
		}
	}
}

// Ensure Session satisfies core.Module at compile time.
var _ core.Module = (*Session)(nil)
