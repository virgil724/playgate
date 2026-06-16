// Package nxbt implements core.InputTarget backed by the NXBT Python daemon
// over a Unix socket or TCP connection. This file contains the public API and
// platform-agnostic logic; the net.Dial call is in dial.go.
package nxbt

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/playgate/playgate-host/internal/core"
	"github.com/playgate/playgate-host/internal/metrics"
)

const (
	// DefaultRateHz is the maximum number of InputCommands forwarded to the
	// daemon per second.  Commands beyond this rate are coalesced (the latest
	// value overwrites queued ones).
	DefaultRateHz = 120

	// pingInterval is the wall-clock interval between keepalive pings.
	pingInterval = 5 * time.Second

	// reconnectBase is the first reconnect back-off delay.
	reconnectBase = 500 * time.Millisecond
	// reconnectMax is the ceiling for exponential back-off.
	reconnectMax = 30 * time.Second
)

// DialFunc is the function used to open a connection to the daemon socket.
// It can be replaced in tests to inject a fake connection.
type DialFunc func(ctx context.Context, socketPath string) (net.Conn, error)

// Target implements core.InputTarget by forwarding InputCommand values to the
// NXBT Python daemon over a Unix socket.
type Target struct {
	log        *slog.Logger
	socketPath string
	rateHz     int

	// status channel is owned by Target; closed when Run exits.
	statusCh chan core.TargetStatus

	coalescer *coalescer

	// latency, when non-nil, records the time from a command being decoded off
	// the WebRTC DataChannel (cmd.Timestamp) to it being written to the daemon
	// socket. Covers the coalescer/rate-limit delay.
	latency *metrics.Histogram

	// daemonLatency, when non-nil, records daemon-local input apply latency
	// reported by nxbtd.py via input_lat messages.
	daemonLatency *metrics.Histogram

	// dial is used to establish the connection. Defaults to dialSocket.
	dial DialFunc

	// connMu guards conn so Send and Run can both access it safely.
	connMu sync.Mutex
	conn   net.Conn // nil when disconnected
}

// Option is a functional option for New.
type Option func(*Target)

// WithRateHz sets the maximum input rate in commands per second.
// Pass 0 to disable rate limiting.
func WithRateHz(hz int) Option {
	return func(t *Target) { t.rateHz = hz }
}

// WithDialFunc overrides the function used to connect to the daemon socket.
// Primarily intended for testing.
func WithDialFunc(fn DialFunc) Option {
	return func(t *Target) { t.dial = fn }
}

// WithLatencyHistogram records, for each command written to the daemon, the
// elapsed time since cmd.Timestamp (when the host decoded it). Pass nil to
// disable (the default).
func WithLatencyHistogram(h *metrics.Histogram) Option {
	return func(t *Target) { t.latency = h }
}

// WithDaemonHistogram records daemon-local receive-to-apply input latency
// reported by nxbtd.py. Pass nil to disable (the default).
func WithDaemonHistogram(h *metrics.Histogram) Option {
	return func(t *Target) { t.daemonLatency = h }
}

// New constructs a Target that will connect to the NXBT daemon at socketPath.
// It does not yet establish the connection; call Start then run Run.
func New(log *slog.Logger, socketPath string, opts ...Option) *Target {
	t := &Target{
		log:        log.With("module", "nxbt"),
		socketPath: socketPath,
		rateHz:     DefaultRateHz,
		statusCh:   make(chan core.TargetStatus, 4),
		dial:       dialSocket,
	}
	for _, o := range opts {
		o(t)
	}
	t.coalescer = newCoalescer(t.rateHz)
	return t
}

// Name implements core.Module.
func (t *Target) Name() string { return "nxbt" }

// Status implements core.InputTarget; the channel is closed when Run exits.
func (t *Target) Status() <-chan core.TargetStatus { return t.statusCh }

// Start implements core.InputTarget. For this target the actual connection is
// established (and re-established) by the Run goroutine; Start is a no-op
// provided to satisfy the interface.
func (t *Target) Start(_ context.Context) error { return nil }

// Send implements core.InputTarget. It pushes cmd into the coalescer;
// the Run loop drains the coalescer and writes to the socket. If there
// is no live connection the command is still accepted into the coalescer
// (it will be sent once reconnected, subject to the rate limit).
// Returns ErrNotConnected if the coalescer cannot be reached.
func (t *Target) Send(cmd core.InputCommand) error {
	t.coalescer.push(cmd)
	return nil
}

// ErrNotConnected is returned by Send when the target is not running.
var ErrNotConnected = errors.New("nxbt: not connected")

// Run implements core.Module. It connects to the NXBT daemon socket,
// forwards coalesced InputCommands, reads status updates, and reconnects
// automatically on failure. Run blocks until ctx is cancelled.
func (t *Target) Run(ctx context.Context) error {
	defer close(t.statusCh)
	t.log.Info("starting nxbt target", "socket", t.socketPath, "rateHz", t.rateHz)

	backoff := reconnectBase
	for {
		t.emitStatus(core.TargetStatusConnecting)

		conn, err := t.dial(ctx, t.socketPath)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			t.log.Warn("connect failed, retrying", "err", err, "backoff", backoff)
			t.emitStatus(core.TargetStatusDisconnected)
			if !sleepWithContext(ctx, backoff) {
				return nil
			}
			backoff = minDuration(backoff*2, reconnectMax)
			continue
		}

		backoff = reconnectBase // reset after successful connect
		t.setConn(conn)
		t.log.Info("connected to nxbt daemon")

		runErr := t.runSession(ctx, conn)
		t.setConn(nil)
		_ = conn.Close()

		if ctx.Err() != nil {
			return nil
		}
		t.log.Warn("session ended, reconnecting", "err", runErr, "backoff", backoff)
		t.emitStatus(core.TargetStatusDisconnected)
		if !sleepWithContext(ctx, backoff) {
			return nil
		}
		backoff = minDuration(backoff*2, reconnectMax)
	}
}

// runSession drives a single connected session until the connection closes or
// ctx is cancelled. It returns the reason the session ended.
func (t *Target) runSession(ctx context.Context, conn net.Conn) error {
	// Start a goroutine to read inbound lines from the daemon.
	type readResult struct {
		line []byte
		err  error
	}
	readCh := make(chan readResult, 8)
	go func() {
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			line := make([]byte, len(scanner.Bytes()))
			copy(line, scanner.Bytes())
			readCh <- readResult{line: line}
		}
		readCh <- readResult{err: scanner.Err()}
		close(readCh)
	}()

	pingTicker := time.NewTicker(pingInterval)
	defer pingTicker.Stop()

	// flushTimer wakes the loop when a rate-limited command becomes sendable.
	// It starts stopped/drained; armed tracks whether a fire is pending so we
	// can Reset it safely (Stop+drain only when already armed).
	flushTimer := time.NewTimer(0)
	if !flushTimer.Stop() {
		<-flushTimer.C
	}
	defer flushTimer.Stop()
	armed := false

	// rearm sets flushTimer to fire after d, draining any stale signal first.
	rearm := func(d time.Duration) {
		if armed && !flushTimer.Stop() {
			select {
			case <-flushTimer.C:
			default:
			}
		}
		flushTimer.Reset(d)
		armed = true
	}

	// flush drains every sendable command from the coalescer. When the next
	// pending command is rate-limited it arms flushTimer for the remaining
	// interval instead of busy-waiting; when nothing is pending it returns and
	// the loop sleeps until the next notify.
	flush := func() error {
		for {
			if cmd, ok := t.coalescer.poll(); ok {
				if err := t.writeInput(conn, cmd); err != nil {
					return fmt.Errorf("write input: %w", err)
				}
				continue
			}
			switch d := t.coalescer.untilReady(); {
			case d < 0:
				return nil // nothing pending; wait for the next notify
			case d == 0:
				continue // became sendable; poll again
			default:
				rearm(d)
				return nil
			}
		}
	}

	// Drain anything pushed before the connection was established.
	if err := flush(); err != nil {
		return err
	}

	var lastPingSent time.Time

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case res, ok := <-readCh:
			if !ok {
				return fmt.Errorf("daemon closed connection")
			}
			if res.err != nil {
				return fmt.Errorf("read from daemon: %w", res.err)
			}
			if err := t.handleInbound(bytes.TrimSpace(res.line)); err != nil {
				t.log.Warn("inbound parse error", "err", err)
			}

		case <-t.coalescer.notifyC():
			if err := flush(); err != nil {
				return err
			}

		case <-flushTimer.C:
			armed = false
			if err := flush(); err != nil {
				return err
			}

		case <-pingTicker.C:
			if time.Since(lastPingSent) >= pingInterval {
				if err := t.writePing(conn); err != nil {
					return fmt.Errorf("write ping: %w", err)
				}
				lastPingSent = time.Now()
			}
		}
	}
}

// handleInbound dispatches a parsed inbound daemon message.
func (t *Target) handleInbound(line []byte) error {
	if len(line) == 0 {
		return nil
	}
	msg, err := decodeInbound(line)
	if err != nil {
		return err
	}
	switch m := msg.(type) {
	case statusMsg:
		t.log.Info("daemon status", "state", m.State, "detail", m.Detail)
		t.emitStatus(wireStateToTarget(m.State))
	case pongMsg:
		t.log.Debug("pong received")
	case inputLatMsg:
		if t.daemonLatency != nil && m.US >= 0 {
			t.daemonLatency.Observe(time.Duration(m.US) * time.Microsecond)
		}
	}
	return nil
}

// writeInput serialises and writes one InputCommand to conn.
func (t *Target) writeInput(conn net.Conn, cmd core.InputCommand) error {
	data, err := encodeInput(inputMsg{
		Type:    msgTypeInput,
		Buttons: cmd.Buttons,
		LX:      cmd.LX,
		LY:      -cmd.LY,
		RX:      cmd.RX,
		RY:      -cmd.RY,
	})
	if err != nil {
		return err
	}
	if _, err = conn.Write(data); err != nil {
		return err
	}
	if t.latency != nil && !cmd.Timestamp.IsZero() {
		t.latency.Observe(time.Since(cmd.Timestamp))
	}
	return nil
}

// writePing sends a ping to conn.
func (t *Target) writePing(conn net.Conn) error {
	data, err := encodePing()
	if err != nil {
		return err
	}
	_, err = conn.Write(data)
	return err
}

// emitStatus sends a status update on the status channel, non-blocking.
func (t *Target) emitStatus(s core.TargetStatus) {
	select {
	case t.statusCh <- s:
	default:
		// Drop if consumer is not keeping up; the next status update will
		// supersede this one.
	}
}

// setConn safely replaces the stored connection handle.
func (t *Target) setConn(c net.Conn) {
	t.connMu.Lock()
	t.conn = c
	t.connMu.Unlock()
}

// wireStateToTarget converts the daemon wire state string to a TargetStatus.
func wireStateToTarget(s wireState) core.TargetStatus {
	switch s {
	case wireStateConnecting:
		return core.TargetStatusConnecting
	case wireStateConnected:
		return core.TargetStatusConnected
	case wireStateDisconnected:
		return core.TargetStatusDisconnected
	default:
		return core.TargetStatusUnknown
	}
}

// sleepWithContext waits d or until ctx is done. Returns false if ctx was
// cancelled before the sleep completed.
func sleepWithContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// minDuration returns the smaller of a and b.
func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
