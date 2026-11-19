package gateway

import (
	"context"
	"fmt"
	"io"
	"strings"

	"go.bug.st/serial"
)

// Espressif's built-in USB Serial/JTAG controller (ESP32-C3/C6/S3...).
const (
	espressifVID   = "303A"
	usbSerialJTAGP = "1001"
)

// openSerial opens the gateway port. name "auto" (or "") picks the single
// Espressif USB Serial/JTAG port present.
func openSerial(_ context.Context, name string) (io.ReadWriteCloser, error) {
	if name == "" || strings.EqualFold(name, "auto") {
		found, err := findGatewayPort()
		if err != nil {
			return nil, err
		}
		name = found
	}
	// DTR/RTS stay low: the USB Serial/JTAG controller maps them to the chip's
	// reset/boot lines, and opening with the default (both high) can reset the
	// gateway. The baud rate is ignored by the USB CDC link.
	p, err := serial.Open(name, &serial.Mode{
		BaudRate:          115200,
		InitialStatusBits: &serial.ModemOutputBits{DTR: false, RTS: false},
	})
	if err != nil {
		return nil, fmt.Errorf("gateway: open %s: %w", name, err)
	}
	return p, nil
}
