package nxbt

import (
	"context"
	"fmt"
	"net"
	"strings"
)

// dialSocket connects to the NXBT daemon. When addr contains a colon but no
// forward-slash (e.g. "192.168.1.5:12345" or "rpi.local:12345") it dials TCP;
// otherwise it dials a Unix domain socket at the given path.
func dialSocket(ctx context.Context, addr string) (net.Conn, error) {
	network := "unix"
	if strings.ContainsRune(addr, ':') && !strings.ContainsRune(addr, '/') {
		network = "tcp"
	}
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s %s: %w", network, addr, err)
	}
	return conn, nil
}
