package module

import (
	"context"
	"log/slog"

	"github.com/playgate/playgate-host/internal/core"
)

// Encoder is a stub H.264 encoder. The real T3 implementation will pipe raw
// frames through an ffmpeg subprocess and emit Annex-B packets. The stub owns
// its output channel and ranges over its input until the input is closed or the
// context is cancelled.
type Encoder struct {
	log     *slog.Logger
	in      <-chan core.VideoFrame
	packets chan core.EncodedPacket
}

// Ensure Encoder satisfies core.Module at compile time.
var _ core.Module = (*Encoder)(nil)

// NewEncoder constructs a stub Encoder reading from in.
func NewEncoder(log *slog.Logger, in <-chan core.VideoFrame) *Encoder {
	return &Encoder{
		log:     log.With("module", "encoder"),
		in:      in,
		packets: make(chan core.EncodedPacket),
	}
}

// Name implements core.Module.
func (e *Encoder) Name() string { return "encoder" }

// Packets returns the receive-only channel of encoded packets. The Encoder owns
// and closes it on exit.
func (e *Encoder) Packets() <-chan core.EncodedPacket { return e.packets }

// Run implements core.Module.
func (e *Encoder) Run(ctx context.Context) error {
	defer close(e.packets)

	e.log.Info("started")
	defer e.log.Info("stopped")

	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-e.in:
			if !ok {
				// Upstream capture closed its channel: clean end of stream.
				return nil
			}
			// Real encoder would feed the frame to ffmpeg and emit packets here.
		}
	}
}
