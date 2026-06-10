//go:build !linux

package v4l2

import (
	"context"
	"errors"
	"log/slog"

	"github.com/playgate/playgate-host/internal/core"
)

// errUnsupported is returned by every device-touching entry point on non-Linux
// platforms. V4L2 is a Linux kernel API; the real implementation lives in the
// `linux`-tagged files. This stub exists so the package compiles (and the pure
// negotiation/format logic stays testable) on Windows and macOS dev machines.
var errUnsupported = errors.New("v4l2 capture is only supported on Linux")

// Source is the non-Linux placeholder for the V4L2 capture source. It satisfies
// core.CaptureSource so wiring code compiles, but Start always fails.
type Source struct {
	log    *slog.Logger
	cfg    Config
	frames chan core.VideoFrame
	errs   chan error
}

var _ core.CaptureSource = (*Source)(nil)

// New validates the config and returns a placeholder Source. The config error
// path is shared with Linux so tests exercise it everywhere.
func New(log *slog.Logger, cfg Config) (*Source, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	return &Source{
		log:    log.With("module", "capture", "device", cfg.Device),
		cfg:    cfg,
		frames: make(chan core.VideoFrame, FrameBufferSize),
		errs:   make(chan error, 1),
	}, nil
}

// Name implements core.Module.
func (s *Source) Name() string { return "capture" }

// Frames implements core.CaptureSource.
func (s *Source) Frames() <-chan core.VideoFrame { return s.frames }

// Errors implements core.CaptureSource.
func (s *Source) Errors() <-chan error { return s.errs }

// Start always fails on non-Linux platforms.
func (s *Source) Start(_ context.Context) error { return errUnsupported }

// Run closes the owned channels and returns the unsupported error.
func (s *Source) Run(_ context.Context) error {
	close(s.frames)
	close(s.errs)
	return errUnsupported
}

// ListDevices is unsupported off Linux.
func ListDevices() ([]DeviceInfo, error) { return nil, errUnsupported }

// QueryDevice is unsupported off Linux.
func QueryDevice(_ string) (DeviceInfo, error) { return DeviceInfo{}, errUnsupported }
