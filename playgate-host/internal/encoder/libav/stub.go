//go:build !(cgo && libav)

// Package libav is the in-process libav (FFmpeg libraries) encoder backend. This
// file is the stub compiled into default (pure-Go) builds; the real
// implementation lives in files tagged `cgo && libav` with the same New signature.
package libav

import (
	"errors"
	"log/slog"

	"github.com/playgate/playgate-host/internal/core"
	"github.com/playgate/playgate-host/internal/encoder/ffmpeg"
)

// ErrUnsupported is returned by New in builds without libav support.
var ErrUnsupported = errors.New("encoder backend libav: built without libav support; rebuild with -tags libav and CGO_ENABLED=1")

// New always fails in builds without libav support. Options are shared with the
// CLI backend (ffmpeg.Options) to keep codec/bitrate semantics identical.
func New(log *slog.Logger, opts ffmpeg.Options, in <-chan core.VideoFrame) (core.VideoEncoder, error) {
	return nil, ErrUnsupported
}
