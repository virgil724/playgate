package module

import (
	"context"
	"log/slog"

	"github.com/playgate/playgate-host/internal/core"
)

// Input is a stub core.InputTarget. The real T5 implementation bridges to the
// NXBT Unix socket to drive a virtual Switch Pro Controller. The stub consumes
// commands from the WebRTC module and reports a fixed status.
type Input struct {
	log    *slog.Logger
	cmds   <-chan core.InputCommand
	status chan core.TargetStatus
}

// Ensure Input satisfies core.InputTarget at compile time.
var _ core.InputTarget = (*Input)(nil)

// NewInput constructs a stub Input target consuming commands from cmds.
func NewInput(log *slog.Logger, cmds <-chan core.InputCommand) *Input {
	return &Input{
		log:    log.With("module", "input"),
		cmds:   cmds,
		status: make(chan core.TargetStatus, 1),
	}
}

// Name implements core.Module.
func (i *Input) Name() string { return "input" }

// Status implements core.InputTarget. The Input target owns and closes it.
func (i *Input) Status() <-chan core.TargetStatus { return i.status }

// Start implements core.InputTarget. The stub has nothing to connect.
func (i *Input) Start(_ context.Context) error { return nil }

// Send implements core.InputTarget. The stub drops the command; the real bridge
// would serialise it onto the NXBT socket.
func (i *Input) Send(_ core.InputCommand) error { return nil }

// Run implements core.Module.
func (i *Input) Run(ctx context.Context) error {
	defer close(i.status)

	i.log.Info("started")
	defer i.log.Info("stopped")

	// Non-blocking initial status report (status is buffered, cap 1).
	i.status <- core.TargetStatusConnected

	for {
		select {
		case <-ctx.Done():
			return nil
		case cmd, ok := <-i.cmds:
			if !ok {
				// Upstream WebRTC closed: no more commands.
				return nil
			}
			if err := i.Send(cmd); err != nil {
				i.log.Warn("send failed", "err", err)
			}
		}
	}
}
