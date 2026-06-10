package logtarget

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/playgate/playgate-host/internal/core"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestSendAndStatus(t *testing.T) {
	tgt := New(testLogger(), 0)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = tgt.Run(ctx); close(done) }()

	// Should report connected.
	select {
	case s := <-tgt.Status():
		if s != core.TargetStatusConnected {
			t.Errorf("status = %v, want connected", s)
		}
	case <-time.After(time.Second):
		t.Fatal("no status reported")
	}

	// Send never errors.
	if err := tgt.Send(core.InputCommand{Buttons: core.ButtonA}); err != nil {
		t.Errorf("Send: %v", err)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	// Status channel must be closed.
	for range tgt.Status() {
	}
}
