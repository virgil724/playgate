//go:build !linux

package nxbt

import (
	"context"
	"fmt"
	"net"
)

// dialUnixSocket on non-Linux platforms returns an error instructing the user
// to run the real target on a Linux host. Tests on Windows/macOS override
// this via the DialFunc field on Target (see target_test.go for the pattern).
func dialUnixSocket(_ context.Context, path string) (net.Conn, error) {
	return nil, fmt.Errorf(
		"unix socket dial is only supported on Linux (path=%q); "+
			"run the NXBT target on a Raspberry Pi / Ubuntu host", path)
}
