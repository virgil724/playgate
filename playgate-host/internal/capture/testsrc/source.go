// Package testsrc implements a core.CaptureSource backed by an ffmpeg lavfi
// "testsrc2" picture with two drawtext overlays: a frame counter / timestamp
// (to spot freezes and skipped frames) and a text file that the overlay input
// target rewrites with the controller state the host is receiving.
//
// ffmpeg writes raw NV12 frames to stdout; Source slices them into
// core.VideoFrame values. It needs an ffmpeg binary but no capture hardware.
//
// Ownership: Source owns the frames and errs channels. Run is the sole writer
// and closes both exactly once on exit, per the core channel contract.
package testsrc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/playgate/playgate-host/internal/core"
)

const frameBufferSize = 2

// idleText is written to the overlay file when it does not exist yet, so
// drawtext always has something readable when ffmpeg starts.
const idleText = "no input yet"

// Config configures the testsrc capture source.
type Config struct {
	// FFmpegPath is the ffmpeg binary; empty means "ffmpeg" on PATH.
	FFmpegPath string
	// Width / Height must be even (NV12 is 4:2:0).
	Width, Height int
	FPS           int
	// FontFile is an optional TrueType font path (normal OS path form).
	FontFile string
	// TextFile is the file drawtext reloads for the input overlay.
	TextFile string
}

// Validate checks the config.
func (c Config) Validate() error {
	if c.Width <= 0 || c.Height <= 0 || c.Width%2 != 0 || c.Height%2 != 0 {
		return fmt.Errorf("testsrc capture: resolution must be positive and even, got %dx%d", c.Width, c.Height)
	}
	if c.FPS <= 0 {
		return fmt.Errorf("testsrc capture: fps must be positive, got %d", c.FPS)
	}
	if c.TextFile == "" {
		return errors.New("testsrc capture: overlay text file must be set")
	}
	if strings.ContainsRune(c.TextFile, '\'') || strings.ContainsRune(c.FontFile, '\'') {
		return errors.New("testsrc capture: font/text file paths must not contain a single quote")
	}
	return nil
}

// EscapeFilterPath converts a normal OS path into the form drawtext expects
// for fontfile=/textfile= when the value is wrapped in single quotes in the
// filtergraph: backslashes become forward slashes and ':' is escaped, e.g.
// C:\Windows\Fonts\consola.ttf -> C\:/Windows/Fonts/consola.ttf.
func EscapeFilterPath(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	return strings.ReplaceAll(p, ":", `\:`)
}

// BuildArgs returns the ffmpeg arguments (without the binary). textFile should
// already be absolute.
func BuildArgs(c Config, textFile string) []string {
	size := fmt.Sprintf("%dx%d", c.Width, c.Height)
	fontOpt := ""
	if c.FontFile != "" {
		fontOpt = "fontfile='" + EscapeFilterPath(c.FontFile) + "':"
	}
	fs := max(14, c.Height/24)
	// Opaque boxes: the moving testsrc2 pattern must not show through, or
	// pixel-based latency/freeze measurements see it as text changes.
	counter := fmt.Sprintf(`drawtext=%stext='frame %%{n}  pts %%{pts\:hms}':x=16:y=16:fontsize=%d:fontcolor=white:box=1:boxcolor=black`, fontOpt, fs)
	input := fmt.Sprintf(`drawtext=%stextfile='%s':reload=1:x=16:y=%d:fontsize=%d:fontcolor=yellow:box=1:boxcolor=black`,
		fontOpt, EscapeFilterPath(textFile), 16+fs*2, fs)
	return []string{
		"-hide_banner", "-nostdin", "-loglevel", "warning",
		"-re",
		"-f", "lavfi", "-i", "testsrc2=size=" + size + ":rate=" + strconv.Itoa(c.FPS),
		"-vf", counter + "," + input,
		"-f", "rawvideo", "-pix_fmt", "nv12", "pipe:1",
	}
}

// Source is a core.CaptureSource producing NV12 frames from ffmpeg.
type Source struct {
	log    *slog.Logger
	cfg    Config
	text   string // absolute overlay path
	frames chan core.VideoFrame
	errs   chan error
}

var _ core.CaptureSource = (*Source)(nil)

// New constructs a Source. The config is validated eagerly.
func New(log *slog.Logger, cfg Config) (*Source, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.FFmpegPath == "" {
		cfg.FFmpegPath = "ffmpeg"
	}
	if log == nil {
		log = slog.Default()
	}
	if cfg.FontFile == "" && runtime.GOOS == "windows" {
		// Windows ffmpeg builds usually have no fontconfig, so drawtext needs an
		// explicit font; fall back to Consolas if present.
		const consola = "C:/Windows/Fonts/consola.ttf"
		if _, err := os.Stat(consola); err == nil {
			cfg.FontFile = consola
		}
	}
	abs, err := filepath.Abs(cfg.TextFile)
	if err != nil {
		return nil, fmt.Errorf("testsrc capture: %w", err)
	}
	return &Source{
		log:    log.With("module", "capture", "source", "testsrc"),
		cfg:    cfg,
		text:   abs,
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

// Start implements core.CaptureSource. It checks ffmpeg is present and makes
// sure the overlay text file exists (drawtext fails if it does not).
func (s *Source) Start(_ context.Context) error {
	if _, err := exec.LookPath(s.cfg.FFmpegPath); err != nil {
		return fmt.Errorf("testsrc capture: ffmpeg not found: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.text), 0o755); err != nil {
		return fmt.Errorf("testsrc capture: %w", err)
	}
	if _, err := os.Stat(s.text); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(s.text, []byte(idleText), 0o644); err != nil {
			return fmt.Errorf("testsrc capture: create overlay file: %w", err)
		}
	}
	s.log.Info("testsrc capture started", "width", s.cfg.Width, "height", s.cfg.Height, "fps", s.cfg.FPS, "overlay", s.text)
	return nil
}

// Run implements core.Module: it runs ffmpeg and forwards frames until the
// context is cancelled or ffmpeg exits, owning and closing both channels.
func (s *Source) Run(ctx context.Context) error {
	defer close(s.frames)
	defer close(s.errs)
	defer s.log.Info("testsrc capture stopped")

	cmd := exec.CommandContext(ctx, s.cfg.FFmpegPath, BuildArgs(s.cfg, s.text)...)
	var stderr tailBuffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("testsrc capture: start ffmpeg: %w", err)
	}

	frameSize := s.cfg.Width * s.cfg.Height * 3 / 2
	var readErr error
	for {
		buf := make([]byte, frameSize)
		if _, readErr = io.ReadFull(stdout, buf); readErr != nil {
			break
		}
		s.forward(ctx, core.VideoFrame{
			Data:        buf,
			PixelFormat: core.PixelFormatNV12,
			Width:       s.cfg.Width,
			Height:      s.cfg.Height,
			Timestamp:   time.Now(),
		})
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return nil
	}
	err = fmt.Errorf("testsrc capture: ffmpeg exited (read: %v, wait: %v): %s", readErr, waitErr, strings.TrimSpace(stderr.String()))
	select {
	case s.errs <- err:
	default:
	}
	return err
}

// forward pushes a frame with a drop-oldest policy.
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

// tailBuffer keeps the last 2 KiB written to it (ffmpeg's stderr).
type tailBuffer struct{ buf bytes.Buffer }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if over := t.buf.Len() - 2048; over > 0 {
		t.buf.Next(over)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return t.buf.String() }
