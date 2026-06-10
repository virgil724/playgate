//go:build linux

package nxbt

import (
	"context"
	"net"
)

// dialUnixSocket connects to the NXBT daemon's Unix domain socket at path.
// It is a thin wrapper so that the connection can be injected in tests via
// dialUnixSocket being replaced; on Linux it uses the real AF_UNIX dial.
func dialUnixSocket(ctx context.Context, path string) (net.Conn, error) {
	d := net.Dialer{}
	return d.DialContext(ctx, "unix", path)
}
