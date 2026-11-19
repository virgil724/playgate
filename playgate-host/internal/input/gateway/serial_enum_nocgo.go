//go:build darwin && !cgo

package gateway

import "errors"

// findGatewayPort cannot enumerate USB ports on macOS without cgo.
func findGatewayPort() (string, error) {
	return "", errors.New("gateway: port auto-detection needs cgo on macOS; set input.gateway.port (e.g. /dev/cu.usbmodem1101)")
}
