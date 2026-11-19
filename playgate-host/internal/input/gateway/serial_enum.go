//go:build !darwin || cgo

package gateway

import (
	"fmt"
	"strings"

	"go.bug.st/serial/enumerator"
)

// findGatewayPort returns the single Espressif USB Serial/JTAG port present.
// (The enumerator needs cgo on macOS; see serial_enum_nocgo.go.)
func findGatewayPort() (string, error) {
	ports, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return "", fmt.Errorf("gateway: list serial ports: %w", err)
	}
	var match []string
	for _, p := range ports {
		if p.IsUSB && strings.EqualFold(p.VID, espressifVID) && strings.EqualFold(p.PID, usbSerialJTAGP) {
			match = append(match, p.Name)
		}
	}
	switch len(match) {
	case 0:
		return "", fmt.Errorf("gateway: no ESP32 USB Serial/JTAG port (VID %s PID %s) found; set input.gateway.port", espressifVID, usbSerialJTAGP)
	case 1:
		return match[0], nil
	}
	return "", fmt.Errorf("gateway: several ESP32 ports found (%s); set input.gateway.port", strings.Join(match, ", "))
}
