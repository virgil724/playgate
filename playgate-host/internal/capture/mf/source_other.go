//go:build !windows

package mf

import (
	"errors"
	"log/slog"

	"github.com/playgate/playgate-host/internal/core"
)

// Config configures the capture.
type Config struct {
	Device        string
	Width, Height int
	FPS           int
}

// New reports that Media Foundation capture needs Windows.
func New(log *slog.Logger, cfg Config) (core.CaptureSource, error) {
	return nil, errors.New("mf capture: capture.source mf is Windows only (use v4l2)")
}
