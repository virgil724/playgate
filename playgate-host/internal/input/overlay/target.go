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

// lineWidth is the fixed width every overlay line is padded (or cut) to, so
// the file always has the same size; see WriteFileInPlace.
const lineWidth = 48

// PadText pads (or truncates) every line of text to lineWidth so successive
// overlay states have identical byte lengths.
func PadText(text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if len(l) > lineWidth {
			l = l[:lineWidth]
		}
		lines[i] = l + strings.Repeat(" ", lineWidth-len(l))
	}
	return strings.Join(lines, "\n")
}

// WriteFileInPlace overwrites path with data without ever replacing, deleting
// or emptying it, because ffmpeg's drawtext (reload=1) re-opens the file on
// every frame and exits on the first failed or empty read:
//
//   - renaming a temp file over it (the usual atomic write) briefly makes the
//     path unopenable on Windows, which ffmpeg reports as "Permission denied";
//   - truncating and rewriting lets ffmpeg read an empty file in between.
//
// Writing fixed-size content (PadText) in place keeps the file present and
// non-empty at all times. A reader racing a write may see one frame mixing old
// and new characters, which the next frame corrects. The file is opened with
// read/write sharing, so this works while ffmpeg has it open.
func WriteFileInPlace(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteAt(data, 0); err != nil {
		f.Close()
		return err
	}
	// Drop any longer leftover tail (e.g. an older, bigger file); this only
	// ever shrinks to len(data) > 0, never to empty.
	if fi, err := f.Stat(); err == nil && fi.Size() > int64(len(data)) {
		if err := f.Truncate(int64(len(data))); err != nil {
			f.Close()
			return err
		}
	}
	return f.Close()
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
	return WriteFileInPlace(t.path, []byte(PadText(FormatText(core.InputCommand{}, time.Now()))))
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
	if err := WriteFileInPlace(t.path, []byte(PadText(FormatText(cmd, recv)))); err != nil {
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
