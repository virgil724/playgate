// Package overlay implements a core.InputTarget that renders the controller
// state it receives as a few lines of text in a file. The testsrc capture
// source burns that file into an ffmpeg-generated picture (drawtext
// textfile=...:reload=1), so the browser shows exactly what the host received:
// a visual end-to-end check of the input path.
//
// Send never blocks and never touches the disk: it stores the latest command.
// Run flushes the latest state to the file at a bounded rate, atomically.
//
// Ownership: Target owns the Status channel; Run is the sole writer and closes
// it on exit, per the core channel contract.
package overlay

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/playgate/playgate-host/internal/core"
)

// flushInterval bounds how often the overlay file is rewritten (~30 Hz is more
// than the 30/60 fps picture can show).
const flushInterval = 25 * time.Millisecond

var buttonNames = []struct {
	mask uint32
	name string
}{
	{core.ButtonA, "A"}, {core.ButtonB, "B"}, {core.ButtonX, "X"}, {core.ButtonY, "Y"},
	{core.ButtonL, "L"}, {core.ButtonR, "R"}, {core.ButtonZL, "ZL"}, {core.ButtonZR, "ZR"},
	{core.ButtonPlus, "+"}, {core.ButtonMinus, "-"}, {core.ButtonHome, "HOME"},
	{core.ButtonCapture, "CAP"}, {core.ButtonLStick, "LS"}, {core.ButtonRStick, "RS"},
	{core.ButtonDpadUp, "UP"}, {core.ButtonDpadDown, "DOWN"},
	{core.ButtonDpadLeft, "LEFT"}, {core.ButtonDpadRight, "RIGHT"},
}

// ButtonList returns the names of the pressed buttons, space separated, or
// "-" when none are pressed.
func ButtonList(buttons uint32) string {
	var names []string
	for _, b := range buttonNames {
		if buttons&b.mask != 0 {
			names = append(names, b.name)
		}
	}
	if len(names) == 0 {
		return "-"
	}
	return strings.Join(names, " ")
}

// FormatText renders the overlay text for a command received at recv. The text
// deliberately avoids '%' and '\' because drawtext expands both in textfile
// content.
func FormatText(cmd core.InputCommand, recv time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "BTN  %s\n", ButtonList(cmd.Buttons))
	fmt.Fprintf(&b, "LS   %+.2f %+.2f\n", cmd.LX, cmd.LY)
	fmt.Fprintf(&b, "RS   %+.2f %+.2f\n", cmd.RX, cmd.RY)
	fmt.Fprintf(&b, "SEQ  %d\n", cmd.Seq)
	fmt.Fprintf(&b, "RECV %s", recv.Format("15:04:05.000"))
	return b.String()
}

// WriteFileAtomic replaces path with data by writing a temp file in the same
// directory and renaming it over the target, so a concurrent reader (ffmpeg
// drawtext reload) never sees a half-written file.
//
// On Windows, renaming over a file that another process holds open without
// FILE_SHARE_DELETE fails with a sharing violation. ffmpeg only keeps the
// file open briefly while reloading, so the rename is retried a few times; if
// it still fails, we fall back to a direct (non-atomic) overwrite so the
// overlay keeps updating rather than going stale.
func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	var rerr error
	for i := 0; i < 5; i++ {
		if rerr = os.Rename(tmpName, path); rerr == nil {
			return nil
		}
		time.Sleep(2 * time.Millisecond)
	}
	os.Remove(tmpName)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("rename failed (%v) and direct write failed: %w", rerr, err)
	}
	return nil
}

// Target is a core.InputTarget that writes controller state to a text file.
type Target struct {
	log    *slog.Logger
	path   string
	status chan core.TargetStatus

	mu    sync.Mutex
	last  core.InputCommand
	recv  time.Time
	dirty bool
}

// Ensure Target satisfies core.InputTarget at compile time.
var _ core.InputTarget = (*Target)(nil)

// New constructs an overlay Target writing to path.
func New(log *slog.Logger, path string) (*Target, error) {
	if path == "" {
		return nil, fmt.Errorf("overlay input: text file path must not be empty")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Target{
		log:    log.With("module", "input", "target", "overlay"),
		path:   path,
		status: make(chan core.TargetStatus, 4),
	}, nil
}

// Name implements core.Module.
func (t *Target) Name() string { return "input" }

// Status implements core.InputTarget; closed when Run exits.
func (t *Target) Status() <-chan core.TargetStatus { return t.status }

// Start implements core.InputTarget. It makes sure the output directory exists
// and writes an initial (idle) state.
func (t *Target) Start(_ context.Context) error {
	if err := os.MkdirAll(filepath.Dir(t.path), 0o755); err != nil {
		return fmt.Errorf("overlay input: %w", err)
	}
	return WriteFileAtomic(t.path, []byte(FormatText(core.InputCommand{}, time.Now())))
}

// Send implements core.InputTarget. It records the latest command; it never
// blocks and never fails.
func (t *Target) Send(cmd core.InputCommand) error {
	t.mu.Lock()
	t.last, t.recv, t.dirty = cmd, time.Now(), true
	t.mu.Unlock()
	return nil
}

func (t *Target) flush() {
	t.mu.Lock()
	if !t.dirty {
		t.mu.Unlock()
		return
	}
	cmd, recv := t.last, t.recv
	t.dirty = false
	t.mu.Unlock()
	if err := WriteFileAtomic(t.path, []byte(FormatText(cmd, recv))); err != nil {
		t.log.Warn("overlay write failed", "err", err)
	}
}

// Run implements core.Module. It flushes the latest state until cancelled and
// owns/closes the Status channel.
func (t *Target) Run(ctx context.Context) error {
	defer close(t.status)
	t.log.Info("overlay input target started", "file", t.path)
	defer t.log.Info("overlay input target stopped")

	select {
	case t.status <- core.TargetStatusConnected:
	default:
	}

	tick := time.NewTicker(flushInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			t.flush()
		}
	}
}
