package config

import (
	"runtime"
	"strings"
	"testing"
)

// capture.source mf needs a device and Windows.
func TestCaptureMFValidation(t *testing.T) {
	c := Default()
	c.Capture.Source = CaptureMF
	c.Capture.Device = ""
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "capture.device") {
		t.Errorf("no device: err %v", err)
	}
	c.Capture.Device = "Live Gamer Ultra-Video"
	err := c.Validate()
	if runtime.GOOS == "windows" {
		if err != nil && strings.Contains(err.Error(), "capture") {
			t.Errorf("valid mf config rejected: %v", err)
		}
	} else if err == nil || !strings.Contains(err.Error(), "Windows only") {
		t.Errorf("mf off Windows: err %v", err)
	}
}
