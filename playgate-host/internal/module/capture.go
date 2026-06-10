// Package module contains stub implementations of the PlayGate Host pipeline
// modules. Each stub follows the uniform lifecycle pattern: it runs its own loop
// in Run, owns its output channels, and exits cleanly when the context is
// cancelled. Real implementations land in T2-T5/T9.
package module

import (
	"context"
	"log/slog"
	"time"

	"github.com/playgate/playgate-host/internal/core"
)

// Capture is a stub core.CaptureSource. The real T2 implementation will read
// frames from a v4l2 device; for now it just runs an idle loop and owns its
// (never-written) output channels so the wiring and shutdown path are exercised.
type Capture struct {
	log    *slog.Logger
	frames chan core.VideoFrame
	errs   chan error
}

// Ensure Capture satisfies core.CaptureSource at compile time.
var _ core.CaptureSource = (*Capture)(nil)

// NewCapture constructs a stub Capture module.
func NewCapture(log *slog.Logger) *Capture {
	return &Capture{
		log:    log.With("module", "capture"),
		frames: make(chan core.VideoFrame),
		errs:   make(chan error),
	}
}

// Name implements core.Module.
func (c *Capture) Name() string { return "capture" }

// Frames implements core.CaptureSource.
func (c *Capture) Frames() <-chan core.VideoFrame { return c.frames }

// Errors implements core.CaptureSource.
func (c *Capture) Errors() <-chan error { return c.errs }

// Start implements core.CaptureSource. The stub has nothing to initialise.
func (c *Capture) Start(_ context.Context) error { return nil }

// Run implements core.Module. It owns frames and errs and closes them on exit.
func (c *Capture) Run(ctx context.Context) error {
	// The producer owns these channels and must close them exactly once on exit.
	defer close(c.frames)
	defer close(c.errs)

	c.log.Info("started")
	defer c.log.Info("stopped")

	// Stub idle loop. Real capture would block on the device here.
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			c.log.Debug("capture heartbeat (stub)")
		}
	}
}
