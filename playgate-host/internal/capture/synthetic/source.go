// Package synthetic implements a pure-Go core.CaptureSource that produces a
// moving test pattern as YUYV (4:2:2) frames. It depends on no operating-system
// capture API, so it runs identically on Windows, macOS, and Linux. It exists so
// the full host pipeline (capture → encoder → WebRTC) can be exercised on a dev
// machine that has no capture card / Switch / ffmpeg-fed hardware.
//
// The pattern is a horizontally-scrolling colour gradient with a moving vertical
// bar, which makes motion (and therefore inter-frame compression and latency)
// visible in the browser.
//
// Ownership: Source owns the frames and errs channels. Run is the sole writer
// and closes both exactly once on exit, per the core channel contract.
package synthetic

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/playgate/playgate-host/internal/core"
)

// frameBufferSize bounds the Frames() channel. A buffer of 2 keeps latency low;
// the producer drops the oldest queued frame when the consumer falls behind.
const frameBufferSize = 2

// Config configures the synthetic capture source.
type Config struct {
	// Width / Height are the generated frame resolution in pixels. Width must be
	// even (YUYV packs two pixels per 4-byte macropixel).
	Width  int
	Height int
	// FPS is the frame generation rate.
	FPS int
}

// Validate checks the config for obviously-wrong values.
func (c Config) Validate() error {
	if c.Width <= 0 || c.Height <= 0 {
		return fmt.Errorf("synthetic capture: resolution must be positive, got %dx%d", c.Width, c.Height)
	}
	if c.Width%2 != 0 {
		return fmt.Errorf("synthetic capture: width must be even for YUYV, got %d", c.Width)
	}
	if c.FPS <= 0 {
		return fmt.Errorf("synthetic capture: fps must be positive, got %d", c.FPS)
	}
	return nil
}

// Source is a pure-Go core.CaptureSource producing YUYV test frames.
type Source struct {
	log    *slog.Logger
	cfg    Config
	frames chan core.VideoFrame
	errs   chan error

	// now is injectable for deterministic tests; nil means time.Now.
	now func() time.Time
}

// Ensure Source satisfies core.CaptureSource at compile time.
var _ core.CaptureSource = (*Source)(nil)

// New constructs a synthetic capture Source. The config is validated eagerly.
func New(log *slog.Logger, cfg Config) (*Source, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	return &Source{
		log:    log.With("module", "capture", "source", "synthetic"),
		cfg:    cfg,
		frames: make(chan core.VideoFrame, frameBufferSize),
		errs:   make(chan error, 1),
		now:    time.Now,
	}, nil
}

// Name implements core.Module.
func (s *Source) Name() string { return "capture" }

// Frames implements core.CaptureSource.
func (s *Source) Frames() <-chan core.VideoFrame { return s.frames }

// Errors implements core.CaptureSource.
func (s *Source) Errors() <-chan error { return s.errs }

// Start implements core.CaptureSource. There is no device to open, so it only
// logs the negotiated geometry.
func (s *Source) Start(_ context.Context) error {
	s.log.Info("synthetic capture started", "width", s.cfg.Width, "height", s.cfg.Height, "fps", s.cfg.FPS)
	return nil
}

// Run implements core.Module. It generates one YUYV frame per tick until the
// context is cancelled, forwarding each with a drop-oldest policy so a slow
// consumer never blocks generation. It owns and closes the channels on exit.
func (s *Source) Run(ctx context.Context) error {
	defer close(s.frames)
	defer close(s.errs)
	defer s.log.Info("synthetic capture stopped")

	interval := time.Second / time.Duration(s.cfg.FPS)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var phase int
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			frame := core.VideoFrame{
				Data:        renderYUYV(s.cfg.Width, s.cfg.Height, phase),
				PixelFormat: core.PixelFormatYUYV,
				Width:       s.cfg.Width,
				Height:      s.cfg.Height,
				Timestamp:   s.now(),
			}
			phase++
			s.forward(ctx, frame)
		}
	}
}

// forward pushes a frame onto the output channel with a drop-oldest policy.
func (s *Source) forward(ctx context.Context, frame core.VideoFrame) {
	select {
	case s.frames <- frame:
		return
	default:
	}
	select {
	case <-s.frames: // drop one stale frame
	default:
	}
	select {
	case s.frames <- frame:
	case <-ctx.Done():
	default:
		s.log.Debug("dropped frame: consumer not keeping up")
	}
}

// renderYUYV builds one packed YUYV 4:2:2 frame of the given geometry. Each
// 4-byte macropixel encodes two horizontal pixels: [Y0 U Y1 V]. The pattern is a
// vertical luma gradient plus a chroma gradient that scrolls horizontally with
// phase, and a bright moving vertical bar to make motion obvious.
func renderYUYV(width, height, phase int) []byte {
	buf := make([]byte, width*height*2)
	barX := (phase * 4) % width // bar position scrolls right over time

	i := 0
	for y := 0; y < height; y++ {
		// Luma rises top-to-bottom so the frame is clearly oriented.
		baseY := byte(16 + (y*200)/height)
		for x := 0; x < width; x += 2 {
			// Chroma scrolls horizontally with phase for visible motion.
			u := byte((x + phase*2) % 256)
			v := byte((y + phase) % 256)

			y0 := baseY
			y1 := baseY
			// Draw a bright vertical bar a few pixels wide.
			if x >= barX && x < barX+8 {
				y0, y1 = 235, 235
			}

			buf[i+0] = y0
			buf[i+1] = u
			buf[i+2] = y1
			buf[i+3] = v
			i += 4
		}
	}
	return buf
}
