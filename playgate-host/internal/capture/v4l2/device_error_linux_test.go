//go:build linux

package v4l2

// Error-path tests that need no real capture hardware: opening a missing
// device node and querying a non-V4L2 file must surface errors, never panic.

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// missingDevicePath returns a path that is guaranteed not to exist.
func missingDevicePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "video0")
}

func TestOpenDeviceMissingNodeReturnsError(t *testing.T) {
	dev, err := openDevice(missingDevicePath(t))
	if err == nil {
		_ = dev.close()
		t.Fatal("openDevice on a missing node: want error, got nil")
	}
	if !errors.Is(err, unix.ENOENT) {
		t.Errorf("openDevice error = %v, want ENOENT in chain", err)
	}
}

func TestQueryDeviceMissingNodeReturnsError(t *testing.T) {
	if _, err := QueryDevice(missingDevicePath(t)); err == nil {
		t.Fatal("QueryDevice on a missing node: want error, got nil")
	}
}

// TestQueryDeviceNonV4L2FileReturnsError opens a plain file: the open succeeds
// but VIDIOC_QUERYCAP must fail with ENOTTY and be reported as an error.
func TestQueryDeviceNonV4L2FileReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-device")
	if err := os.WriteFile(path, []byte("plain file"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := QueryDevice(path)
	if err == nil {
		t.Fatal("QueryDevice on a regular file: want error, got nil")
	}
	if !errors.Is(err, unix.ENOTTY) {
		t.Errorf("QueryDevice error = %v, want ENOTTY in chain", err)
	}
}

// TestSourceStartMissingDeviceReturnsError drives the public capture source:
// Start against a missing /dev/video* must return an error synchronously
// (so the supervisor can report it) rather than panic or hang.
func TestSourceStartMissingDeviceReturnsError(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Device = missingDevicePath(t)
	src, err := New(slog.New(slog.DiscardHandler), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := src.Start(context.Background()); err == nil {
		t.Fatal("Start with a missing device: want error, got nil")
	}
}
