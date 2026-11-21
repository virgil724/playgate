//go:build !(windows && cgo && libav)

package wasapi

import (
	"context"
	"errors"
	"log/slog"

	"github.com/playgate/playgate-host/internal/audio/opus"
)

// Config configures the source.
type Config struct {
	Device    string
	Bitrate   int
	FrameMS   float64
	Exclusive bool
}

// ErrUnavailable is returned outside the Windows libav build.
var ErrUnavailable = errors.New("audio.format wasapi needs the Windows host built with -tags libav (it encodes Opus in-process); use dshow otherwise")

// Source is unavailable in this build; New never returns one.
type Source struct{}

// New reports that this build has no WASAPI source.
func New(log *slog.Logger, cfg Config) (*Source, error) { return nil, ErrUnavailable }

func (*Source) Name() string                { return "audio" }
func (*Source) Run(context.Context) error   { return ErrUnavailable }
func (*Source) Packets() <-chan opus.Packet { return nil }
