package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWebRTCGCCConfig(t *testing.T) {
	if Default().WebRTC.GCC {
		t.Fatal("webrtc.gcc must default to false")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("webrtc:\n  gcc: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.WebRTC.GCC {
		t.Error("webrtc.gcc: true not applied")
	}
}
