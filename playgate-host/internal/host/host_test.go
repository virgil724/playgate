package host

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"testing"
	"time"

	"github.com/playgate/playgate-host/internal/config"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestRunShutsDownOnCancel verifies the core T1 acceptance criterion: starting
// all stub modules then cancelling the context makes Run return cleanly within a
// deadline, with no leaked goroutines.
func TestRunShutsDownOnCancel(t *testing.T) {
	before := runtime.NumGoroutine()

	h := New(discardLogger(), config.Default())

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- h.Run(ctx)
	}()

	// Let the modules spin up, then trigger shutdown.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error on clean shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within deadline after cancel")
	}

	// Give the scheduler a moment to reap finished goroutines, then assert we
	// did not leak relative to the baseline.
	assertNoLeak(t, before)
}

// TestRunReturnsImmediatelyOnAlreadyCancelledContext ensures a pre-cancelled
// context produces a prompt clean exit.
func TestRunReturnsImmediatelyOnAlreadyCancelledContext(t *testing.T) {
	h := New(discardLogger(), config.Default())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly for pre-cancelled context")
	}
}

func assertNoLeak(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		runtime.GC()
		after := runtime.NumGoroutine()
		// Allow a small slack for runtime/background goroutines.
		if after <= before+1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak: before=%d after=%d", before, after)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
