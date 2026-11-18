package overlay

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/playgate/playgate-host/internal/core"
)

func TestFormatText(t *testing.T) {
	recv := time.Date(2026, 1, 2, 3, 4, 5, 678_000_000, time.UTC)
	got := FormatText(core.InputCommand{
		Seq: 42, Buttons: core.ButtonA | core.ButtonZR | core.ButtonDpadLeft,
		LX: 0.5, LY: -1, RX: 0, RY: 0.25,
	}, recv)
	want := "BTN  A ZR LEFT\nLS   +0.50 -1.00\nRS   +0.00 +0.25\nSEQ  42\nRECV 03:04:05.678"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if strings.ContainsAny(got, `%\`) {
		t.Error("overlay text must not contain drawtext-special characters")
	}
	if !strings.HasPrefix(FormatText(core.InputCommand{}, recv), "BTN  -\n") {
		t.Error("no buttons should render as '-'")
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "o.txt")
	for _, content := range []string{"one", "two longer", "3"} {
		if err := WriteFileAtomic(p, []byte(content)); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(p)
		if err != nil || string(b) != content {
			t.Fatalf("read = %q, %v; want %q", b, err, content)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temp files left behind: %v", entries)
	}
}

func TestTargetFlushesLatest(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.txt")
	tg, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)), p)
	if err != nil {
		t.Fatal(err)
	}
	if err := tg.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = tg.Run(ctx); close(done) }()

	_ = tg.Send(core.InputCommand{Seq: 7, Buttons: core.ButtonX})
	deadline := time.Now().Add(2 * time.Second)
	for {
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), "SEQ  7") && strings.Contains(string(b), "BTN  X") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("overlay not updated, file = %q", b)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	for range tg.Status() {
	}
}

func TestNewRejectsEmptyPath(t *testing.T) {
	if _, err := New(nil, ""); err == nil {
		t.Error("expected error for empty path")
	}
}
