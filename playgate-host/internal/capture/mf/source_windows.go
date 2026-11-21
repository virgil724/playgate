//go:build windows

package mf

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	"github.com/virgil724/caplat/capture"
	camf "github.com/virgil724/caplat/capture/mf"

	"github.com/playgate/playgate-host/internal/core"
)

const (
	frameBufferSize = 2
	stallTimeout    = 2 * time.Second
	restartBackoff  = time.Second
)

// Config configures the capture.
type Config struct {
	Device        string // Media Foundation device name, e.g. "Live Gamer Ultra-Video"
	Width, Height int    // must be even (NV12)
	FPS           int
}

// Source is a core.CaptureSource producing NV12 frames.
type Source struct {
	log    *slog.Logger
	cfg    Config
	frames chan core.VideoFrame
	errs   chan error
}

var _ core.CaptureSource = (*Source)(nil)

// New constructs a Source.
func New(log *slog.Logger, cfg Config) (*Source, error) {
	if cfg.Device == "" {
		return nil, errors.New("mf capture: capture.device is required")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width%2 != 0 || cfg.Height%2 != 0 || cfg.FPS <= 0 {
		return nil, fmt.Errorf("mf capture: bad mode %dx%d@%d", cfg.Width, cfg.Height, cfg.FPS)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Source{
		log:    log.With("module", "capture", "source", "mf"),
		cfg:    cfg,
		frames: make(chan core.VideoFrame, frameBufferSize),
		errs:   make(chan error, 1),
	}, nil
}

// Name implements core.Module.
func (s *Source) Name() string { return "capture" }

// Frames implements core.CaptureSource.
func (s *Source) Frames() <-chan core.VideoFrame { return s.frames }

// Errors implements core.CaptureSource.
func (s *Source) Errors() <-chan error { return s.errs }

// Start implements core.CaptureSource: it checks the device is present.
func (s *Source) Start(_ context.Context) error {
	devs, err := camf.ListDevices()
	if err != nil {
		return fmt.Errorf("mf capture: list devices: %w", err)
	}
	if !slices.Contains(devs, s.cfg.Device) {
		return fmt.Errorf("mf capture: no device %q; Media Foundation has %q", s.cfg.Device, devs)
	}
	s.log.Info("mf capture started", "device", s.cfg.Device, "width", s.cfg.Width, "height", s.cfg.Height, "fps", s.cfg.FPS)
	return nil
}

// Run implements core.Module. It captures until ctx ends, reopening the
// device after an error or a stall (no frame for stallTimeout), and owns
// and closes both channels.
func (s *Source) Run(ctx context.Context) error {
	defer close(s.frames)
	defer close(s.errs)
	defer s.log.Info("capture stopped")
	for {
		if err := s.runOnce(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("mf capture ended; will restart", "err", err)
		}
		t := time.NewTimer(restartBackoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

func (s *Source) runOnce(ctx context.Context) error {
	freq := capture.QPCFrequency()
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	var badFormat atomic.Value // string, the first unusable format seen

	src, err := camf.Start(camf.Config{
		Device: s.cfg.Device, Width: s.cfg.Width, Height: s.cfg.Height,
		FPS: float64(s.cfg.FPS), Format: "NV12",
	}, capture.QPC, freq, func(f capture.Frame) {
		last.Store(time.Now().UnixNano())
		if f.FourCC != "NV12" || f.Data == nil {
			badFormat.CompareAndSwap(nil, f.FourCC)
			return
		}
		data, err := TightNV12(f.Data, f.Pitch, f.Width, f.Height)
		if err != nil {
			s.log.Debug("dropped frame", "err", err)
			return
		}
		s.forward(ctx, core.VideoFrame{
			Data: data, PixelFormat: core.PixelFormatNV12,
			Width: f.Width, Height: f.Height, Timestamp: time.Now(),
		})
	})
	if err != nil {
		return err
	}
	defer src.Stop()
	m := src.Mode
	s.log.Info("mf capture mode", "format", m.FourCC, "width", m.Width, "height", m.Height, "fps", m.FPS)

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-src.Done():
			if err == nil {
				err = errors.New("capture stopped")
			}
			return err
		case t := <-tick.C:
			if v := badFormat.Load(); v != nil {
				return fmt.Errorf("mf capture: device delivers %v, not NV12", v)
			}
			if silent := t.Sub(time.Unix(0, last.Load())); silent > stallTimeout {
				return fmt.Errorf("mf capture: no frame for %s", silent.Round(time.Millisecond))
			}
		}
	}
}

// forward pushes a frame, dropping the oldest queued one if the consumer
// is behind.
func (s *Source) forward(ctx context.Context, frame core.VideoFrame) {
	select {
	case s.frames <- frame:
		return
	default:
	}
	select {
	case <-s.frames:
	default:
	}
	select {
	case s.frames <- frame:
	case <-ctx.Done():
	default:
		s.log.Debug("dropped frame: consumer not keeping up")
	}
}
