package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOverlaysDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// Only override a couple of fields; the rest should keep defaults.
	content := "capture:\n  width: 1920\n  height: 1080\nsignaling:\n  room_id: stream42\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Capture.Width != 1920 || cfg.Capture.Height != 1080 {
		t.Errorf("resolution not applied: %dx%d", cfg.Capture.Width, cfg.Capture.Height)
	}
	if cfg.Capture.FPS != Default().Capture.FPS {
		t.Errorf("default FPS not preserved: got %d", cfg.Capture.FPS)
	}
	if cfg.Signaling.RoomID != "stream42" {
		t.Errorf("room_id not applied: %q", cfg.Signaling.RoomID)
	}
}

func TestLoadRejectsInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("capture:\n  width: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected validation error for zero width, got nil")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected error for missing file")
	}
}
