package module

import (
	"context"
	"log/slog"

	"github.com/playgate/playgate-host/internal/core"
)

// WebRTC is a stub for the Pion WebRTC module. The real T4 implementation pushes
// encoded packets onto a media track and decodes controller commands from an
// unreliable/unordered DataChannel. The stub consumes encoded packets and owns
// the commands channel that feeds the InputTarget.
type WebRTC struct {
	log      *slog.Logger
	packets  <-chan core.EncodedPacket
	commands chan core.InputCommand
}

// Ensure WebRTC satisfies core.Module at compile time.
var _ core.Module = (*WebRTC)(nil)

// NewWebRTC constructs a stub WebRTC module reading encoded packets from in.
func NewWebRTC(log *slog.Logger, in <-chan core.EncodedPacket) *WebRTC {
	return &WebRTC{
		log:      log.With("module", "webrtc"),
		packets:  in,
		commands: make(chan core.InputCommand),
	}
}

// Name implements core.Module.
func (w *WebRTC) Name() string { return "webrtc" }

// Commands returns the receive-only channel of decoded controller commands. The
// WebRTC module owns and closes it on exit.
func (w *WebRTC) Commands() <-chan core.InputCommand { return w.commands }

// Run implements core.Module.
func (w *WebRTC) Run(ctx context.Context) error {
	defer close(w.commands)

	w.log.Info("started")
	defer w.log.Info("stopped")

	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-w.packets:
			if !ok {
				// Upstream encoder closed: no more media to push.
				return nil
			}
			// Real implementation would write the packet to the media track and
			// read DataChannel messages into w.commands.
		}
	}
}
