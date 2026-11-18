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
	if err := os.WriteFile(path, []byte("encoder:\n  backend: libav\nwebrtc:\n  gcc: true\n"), 0o600); err != nil {
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

func TestVideoTransportValidate(t *testing.T) {
	c := Default()
	if c.WebRTC.VideoTransport != VideoTransportTrack || c.WebRTC.VideoFragmentBytes != 1200 {
		t.Fatalf("defaults wrong: %+v", c.WebRTC)
	}
	c.WebRTC.VideoTransport = "bogus"
	if c.Validate() == nil {
		t.Fatal("bogus transport must be rejected")
	}
	c.WebRTC.VideoTransport = VideoTransportAuto
	c.WebRTC.VideoFragmentBytes = 100
	if c.Validate() == nil {
		t.Fatal("fragment size out of range must be rejected")
	}
	c.WebRTC.VideoFragmentBytes = 1200
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestWebRTCGCCRequiresLibav(t *testing.T) {
	c := Default()
	c.WebRTC.GCC = true
	c.Encoder.Backend = BackendCLI
	if err := c.Validate(); err == nil {
		t.Fatal("gcc with cli backend must be rejected")
	}
	c.Encoder.Backend = BackendLibav
	if err := c.Validate(); err != nil {
		t.Fatalf("gcc with libav: %v", err)
	}
}
