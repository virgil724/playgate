// Package logtarget implements a core.InputTarget that logs received commands
// instead of forwarding them to a real device. It is the dev-mode counterpart to
// internal/input/nxbt: it lets the full host pipeline run on a machine with no
// NXBT daemon / Switch, so the input path (viewer → session gate → target) can be
// exercised end to end without hardware.
//
// Ownership: Target owns the Status channel; Run is the sole writer and closes
// it on exit, per the core channel contract.
package logtarget

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/playgate/playgate-host/internal/core"
)

// Target is a core.InputTarget that logs commands rather than driving hardware.
type Target struct {
	log    *slog.Logger
	status chan core.TargetStatus

	// sampleEvery throttles per-command logging to one in N to avoid flooding
	// the log at 60 Hz. Zero/negative means log every command.
	sampleEvery int64
	count       atomic.Int64
}

// Ensure Target satisfies core.InputTarget at compile time.
var _ core.InputTarget = (*Target)(nil)

// New constructs a log-only Target. sampleEvery throttles command logging to one
// line per N commands (e.g. 60 ≈ one line/sec at 60 Hz). Pass 0 to log every
// command.
func New(log *slog.Logger, sampleEvery int) *Target {
	if log == nil {
		log = slog.Default()
	}
	return &Target{
		log:         log.With("module", "input", "target", "log"),
		status:      make(chan core.TargetStatus, 4),
		sampleEvery: int64(sampleEvery),
	}
}

// Name implements core.Module.
func (t *Target) Name() string { return "input" }

// Status implements core.InputTarget; the channel is closed when Run exits.
func (t *Target) Status() <-chan core.TargetStatus { return t.status }

// Start implements core.InputTarget. Nothing to connect.
func (t *Target) Start(_ context.Context) error { return nil }

// Send implements core.InputTarget. It logs (sampled) the command and never
// fails.
func (t *Target) Send(cmd core.InputCommand) error {
	n := t.count.Add(1)
	if t.sampleEvery <= 0 || n%t.sampleEvery == 0 {
		t.log.Info("input command",
			"buttons", cmd.Buttons,
			"lx", cmd.LX, "ly", cmd.LY,
			"rx", cmd.RX, "ry", cmd.RY,
			"seq", n)
	}
	return nil
}

// Run implements core.Module. It reports a connected status, then blocks until
// the context is cancelled. It owns and closes the Status channel.
func (t *Target) Run(ctx context.Context) error {
	defer close(t.status)
	t.log.Info("log input target started")
	defer t.log.Info("log input target stopped")

	select {
	case t.status <- core.TargetStatusConnected:
	default:
	}

	<-ctx.Done()
	return nil
}
