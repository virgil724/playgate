// Package host wires the PlayGate Host modules into a single runnable pipeline
// and supervises their lifecycle. It is kept separate from package main so the
// wiring and shutdown behaviour can be unit-tested.
//
// Data flow (T6 end-to-end):
//
//	capture ─VideoFrame─▶ encoder ─EncodedPacket─▶ videoRouter ─▶ active rtc.Peer ─▶ viewer
//	                                                                     │
//	viewer ─InputCommand(binary)─▶ rtc.Peer.Commands ─▶ session.Gate ─▶ InputTarget ─▶ Switch
//	                                                     session.Events ─▶ rtc.Peer control channel (JSON)
//
// The capture→encoder half runs once for the process lifetime. The WebRTC half
// is per-viewer: the connection manager builds a fresh rtc.Peer for each viewer
// that connects through the signaling Worker, and tears it down (so the next
// viewer can connect) when the connection drops.
package host

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/playgate/playgate-host/internal/config"
	"github.com/playgate/playgate-host/internal/core"
	"github.com/playgate/playgate-host/internal/metrics"
	"github.com/playgate/playgate-host/internal/session"
)

// Deps are the wired-up pipeline collaborators. New builds these from config;
// tests inject fakes directly via NewWithDeps.
type Deps struct {
	Capture core.CaptureSource
	Encoder Encoder
	Input   core.InputTarget
	// Session is the single-controller gate; nil disables gating (pass-through).
	Session *session.Manager
	// Connect runs the per-viewer WebRTC connection loop against the signaling
	// Worker. It blocks until ctx is cancelled. The videoRouter and input/session
	// plumbing are passed in so each new Peer is wired identically.
	Connect ConnectFunc
}

// Encoder is the subset of the ffmpeg encoder the host needs: a core.Module that
// also exposes its encoded-packet output.
type Encoder interface {
	core.Module
	Packets() <-chan core.EncodedPacket
}

// ConnectFunc runs the viewer connection lifecycle until ctx is cancelled. It is
// the seam tests replace to avoid real WebRTC/signaling.
type ConnectFunc func(ctx context.Context, router *VideoRouter, sink InputSink) error

// InputSink receives gated input commands and session-event delivery. The
// connection manager hands each new Peer an InputSink so commands flow to the
// session gate and target, and session events flow back to the control channel.
type InputSink interface {
	// HandleCommands consumes a peer's raw command channel for the lifetime of a
	// viewer connection (until raw closes or ctx is cancelled), passing accepted
	// commands to the input target. sendControl is used to deliver session events
	// (JSON) to that viewer's control channel; it may be nil if unsupported.
	HandleCommands(ctx context.Context, raw <-chan core.InputCommand, sendControl func([]byte) error)
}

// Host owns the wired-up pipeline and runs it as a supervised group.
type Host struct {
	log     *slog.Logger
	cfg     config.Config
	deps    Deps
	metrics *metrics.Collector
	session *session.Manager // nil when gating is disabled
}

// New constructs a Host with real modules built from cfg. It returns an error if
// any module cannot be constructed (e.g. invalid capture/encoder config).
func New(log *slog.Logger, cfg config.Config) (*Host, error) {
	if log == nil {
		log = slog.Default()
	}
	mc := metrics.NewCollector()

	deps, err := buildDeps(log, cfg, mc)
	if err != nil {
		return nil, err
	}
	return NewWithDeps(log, cfg, deps, mc), nil
}

// NewWithDeps constructs a Host from pre-built dependencies. It is the seam used
// by integration tests to inject synthetic capture, a fake encoder, a loopback
// connect function, and a fake input target. metrics may be nil.
func NewWithDeps(log *slog.Logger, cfg config.Config, deps Deps, mc *metrics.Collector) *Host {
	if log == nil {
		log = slog.Default()
	}
	if mc == nil {
		mc = metrics.NewCollector()
	}
	return &Host{log: log, cfg: cfg, deps: deps, metrics: mc, session: deps.Session}
}

// Metrics returns the host's latency collector (mainly for tests / inspection).
func (h *Host) Metrics() *metrics.Collector { return h.metrics }

// Run starts every module and blocks until the context is cancelled or any
// module returns a fatal (non-context) error. On any exit the shared context is
// cancelled so the rest shut down too.
func (h *Host) Run(ctx context.Context) error {
	h.log.Info("host starting",
		"capture", h.cfg.CaptureSourceKind(),
		"input", h.cfg.InputTargetKind(),
		"session_enabled", h.cfg.Session.Enabled,
		"room", h.cfg.Signaling.RoomID)

	g, gctx := errgroup.WithContext(ctx)

	// 1. Capture must Start (open the device / init the generator) before Run.
	if err := h.deps.Capture.Start(gctx); err != nil {
		return fmt.Errorf("host: start capture: %w", err)
	}

	// 2. The video router fans the encoder's packets to the currently-active
	// viewer Peer (set by the connection manager).
	router := NewVideoRouter(h.log, h.metrics)

	// 3. Persistent media pipeline: capture, encoder, router, input target.
	g.Go(func() error { return h.deps.Capture.Run(gctx) })
	g.Go(func() error { return h.deps.Encoder.Run(gctx) })
	g.Go(func() error { return h.deps.Input.Run(gctx) })
	g.Go(func() error {
		// Tap capture errors so a card-unplug is visible (non-fatal).
		drainErrors(gctx, h.log, "capture", h.deps.Capture.Errors())
		return nil
	})
	g.Go(func() error {
		router.Run(gctx, h.deps.Encoder.Packets())
		return nil
	})
	g.Go(func() error {
		drainStatus(gctx, h.log, h.deps.Input.Status())
		return nil
	})

	// 4. Latency reporter.
	g.Go(func() error {
		h.metrics.RunReporter(gctx, h.log,
			time.Duration(h.cfg.Metrics.ReportIntervalSeconds)*time.Second)
		return nil
	})

	// 4b. Session manager (only when gating is enabled).
	if h.session != nil {
		g.Go(func() error { return h.session.Run(gctx) })
	}

	// 5. Per-viewer WebRTC connection loop.
	sink := h.newInputSink()
	g.Go(func() error {
		if h.deps.Connect == nil {
			<-gctx.Done()
			return nil
		}
		return h.deps.Connect(gctx, router, sink)
	})

	err := g.Wait()
	if err != nil && err != context.Canceled && ctx.Err() == nil {
		h.log.Error("host stopping due to module error", "err", err)
		return err
	}
	h.log.Info("host stopped cleanly")
	return nil
}

// drainErrors logs non-fatal module errors until the channel closes.
func drainErrors(ctx context.Context, log *slog.Logger, module string, errs <-chan error) {
	for {
		select {
		case <-ctx.Done():
			return
		case err, ok := <-errs:
			if !ok {
				return
			}
			if err != nil {
				log.Warn("module error", "module", module, "err", err)
			}
		}
	}
}

// drainStatus logs input-target connection status changes until close.
func drainStatus(ctx context.Context, log *slog.Logger, status <-chan core.TargetStatus) {
	for {
		select {
		case <-ctx.Done():
			return
		case s, ok := <-status:
			if !ok {
				return
			}
			log.Info("input target status", "state", s.String())
		}
	}
}
