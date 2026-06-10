// Package host wires the PlayGate Host modules into a single runnable pipeline
// and supervises their lifecycle. It is kept separate from package main so the
// wiring and shutdown behaviour can be unit-tested.
package host

import (
	"context"
	"log/slog"

	"golang.org/x/sync/errgroup"

	"github.com/playgate/playgate-host/internal/config"
	"github.com/playgate/playgate-host/internal/core"
	"github.com/playgate/playgate-host/internal/module"
)

// Host owns the wired-up pipeline modules and runs them as a group.
type Host struct {
	log     *slog.Logger
	modules []core.Module
}

// New constructs a Host with the standard pipeline wired together:
//
//	capture ─frames─▶ encoder ─packets─▶ webrtc ─commands─▶ input
//	                                     session (control plane)
//
// The cfg is accepted now so module constructors can consume it as they grow
// real behaviour in later tasks; T1 stubs ignore most of it.
func New(log *slog.Logger, cfg config.Config) *Host {
	_ = cfg // reserved for downstream modules (T2-T9)

	capture := module.NewCapture(log)
	encoder := module.NewEncoder(log, capture.Frames())
	webrtc := module.NewWebRTC(log, encoder.Packets())
	input := module.NewInput(log, webrtc.Commands())
	session := module.NewSession(log)

	return &Host{
		log: log,
		// Order is informational only; all modules run concurrently.
		modules: []core.Module{capture, encoder, webrtc, input, session},
	}
}

// Run starts every module and blocks until the context is cancelled or any
// module returns a fatal (non-context) error. When any module exits, the shared
// context is cancelled so the rest shut down too. Run returns the first
// non-context error encountered, or nil on a clean context-cancelled shutdown.
func (h *Host) Run(ctx context.Context) error {
	h.log.Info("host starting", "modules", len(h.modules))

	g, gctx := errgroup.WithContext(ctx)
	for _, m := range h.modules {
		m := m
		g.Go(func() error {
			return m.Run(gctx)
		})
	}

	err := g.Wait()
	// A cancelled parent context is the normal shutdown signal, not an error.
	if err != nil && err != context.Canceled && ctx.Err() == nil {
		h.log.Error("host stopping due to module error", "err", err)
		return err
	}

	h.log.Info("host stopped cleanly")
	return nil
}
