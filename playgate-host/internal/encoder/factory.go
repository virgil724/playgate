// Package encoder selects a video encoder backend (ffmpeg CLI or in-process libav).
package encoder

import (
	"fmt"
	"log/slog"

	"github.com/playgate/playgate-host/internal/config"
	"github.com/playgate/playgate-host/internal/core"
	"github.com/playgate/playgate-host/internal/encoder/ffmpeg"
	"github.com/playgate/playgate-host/internal/encoder/libav"
)

// New constructs the encoder backend named by backend ("cli", "libav" or "" for
// the default cli). Both backends share ffmpeg.Options so codec/bitrate/GOP
// semantics stay identical; the libav backend is only available in builds with
// `-tags libav` and CGO_ENABLED=1, otherwise selecting it returns an error.
func New(backend string, log *slog.Logger, opts ffmpeg.Options, in <-chan core.VideoFrame) (core.VideoEncoder, error) {
	switch backend {
	case "", config.BackendCLI:
		return ffmpeg.New(log, opts, in)
	case config.BackendLibav:
		return libav.New(log, opts, in)
	default:
		return nil, fmt.Errorf("encoder: unknown backend %q", backend)
	}
}
