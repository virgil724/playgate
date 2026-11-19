// Package gateway implements a core.InputTarget that drives the PlayGate
// Switch 2 gateway (an ESP32-C6 emulating a Pro Controller 2 over BLE) through
// its USB serial link. Pairing and the Bluetooth side live in the firmware; the
// host only streams controller state.
//
// The gateway neutralises the controller when its input source has been quiet
// for 100 ms, so a held state is re-sent every keepalive interval; a neutral
// state is sent once. Every frame carries a fresh sequence number, since the
// gateway drops frames whose sequence is not newer than the last one it
// accepted.
//
// Ownership: Target owns the Status channel; Run is the sole writer and closes
// it on exit, per the core channel contract.
package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/playgate/playgate-host/internal/core"
	"github.com/playgate/playgate-host/internal/input/coalesce"
	"github.com/playgate/playgate-host/internal/metrics"
)

const (
	// DefaultRateHz spaces queued button edges about 6.7 ms apart. The
	// firmware reports a new input at most every 5 ms (rounded up to its 1 ms
	// tick, so 5-6 ms), so two edges never merge into one report. With older
	// firmware that reports at a fixed 100 Hz, use 90.
	// the gateway's 10 ms (100 Hz) report period, so two edges do not land in
	// one report and merge.
	DefaultRateHz = 150
	// DefaultKeepalive re-sends a held (non-neutral) state well inside the
	// gateway's 100 ms input timeout.
	DefaultKeepalive = 40 * time.Millisecond

	reconnectBase = 250 * time.Millisecond
	reconnectMax  = 5 * time.Second

	// maxEncodedFrame bounds a frame read from the gateway (the largest valid
	// one, status, is 27 bytes encoded).
	maxEncodedFrame = 64
)

// OpenFunc opens the gateway's serial link.
type OpenFunc func(ctx context.Context) (io.ReadWriteCloser, error)

// Target implements core.InputTarget for the Switch 2 gateway.
type Target struct {
	log       *slog.Logger
	portName  string
	rateHz    int
	keepalive time.Duration
	open      OpenFunc

	statusCh  chan core.TargetStatus
	coalescer *coalesce.Coalescer

	// latency records cmd.Timestamp (decoded off the DataChannel) to the
	// frame being written to the serial port; it covers the rate-limit delay.
	latency *metrics.Histogram
	// apply records the gateway's own receive-to-HID-submit time.
	apply *metrics.Histogram

	// seq is the last sequence number sent. It starts from the wall clock in
	// milliseconds so a restarted host is still "newer" than the frames the
	// gateway accepted from the previous run (it ignores older sequences for a
	// 300 ms lease); the host sends far fewer than 1000 frames per second.
	seq uint32
}

// Option configures a Target.
type Option func(*Target)

// WithRateHz caps how often queued button edges are forwarded. 0 disables it.
func WithRateHz(hz int) Option { return func(t *Target) { t.rateHz = hz } }

// WithKeepalive sets how often a held state is re-sent.
func WithKeepalive(d time.Duration) Option {
	return func(t *Target) {
		if d > 0 {
			t.keepalive = d
		}
	}
}

// WithOpenFunc replaces the serial port opener (tests).
func WithOpenFunc(fn OpenFunc) Option { return func(t *Target) { t.open = fn } }

// WithLatencyHistogram records host-side input latency.
func WithLatencyHistogram(h *metrics.Histogram) Option { return func(t *Target) { t.latency = h } }

// WithApplyHistogram records the gateway-reported receive-to-submit time.
func WithApplyHistogram(h *metrics.Histogram) Option { return func(t *Target) { t.apply = h } }

// New returns a gateway target for the given serial port ("auto" finds the
// ESP32-C6 USB Serial/JTAG port).
func New(log *slog.Logger, port string, opts ...Option) *Target {
	t := &Target{
		log:       log.With("module", "input-gateway"),
		portName:  port,
		rateHz:    DefaultRateHz,
		keepalive: DefaultKeepalive,
		statusCh:  make(chan core.TargetStatus, 4),
		seq:       uint32(time.Now().UnixMilli()),
	}
	t.open = func(ctx context.Context) (io.ReadWriteCloser, error) { return openSerial(ctx, t.portName) }
	for _, o := range opts {
		o(t)
	}
	t.coalescer = coalesce.New(t.rateHz)
	return t
}

// Name implements core.Module.
func (t *Target) Name() string { return "gateway" }

// Status implements core.InputTarget; the channel is closed when Run exits.
func (t *Target) Status() <-chan core.TargetStatus { return t.statusCh }

// Start implements core.InputTarget. The port is opened (and reopened) by Run.
func (t *Target) Start(context.Context) error { return nil }

// Send implements core.InputTarget. It never blocks: the command is queued in
// the coalescer and written by Run once the port is open.
func (t *Target) Send(cmd core.InputCommand) error {
	t.coalescer.Push(cmd)
	return nil
}

// Run implements core.Module. It opens the serial port, streams commands and
// reads status reports, reopening the port with backoff when it fails (cable
// pulled, gateway reset). It returns when ctx is cancelled.
func (t *Target) Run(ctx context.Context) error {
	defer close(t.statusCh)
	t.log.Info("starting gateway target", "port", t.portName, "rateHz", t.rateHz, "keepalive", t.keepalive)

	backoff := reconnectBase
	var last core.InputCommand // re-sent on reconnect so a held state resumes
	for {
		t.emitStatus(core.TargetStatusConnecting)
		port, err := t.open(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			t.log.Warn("open gateway port failed, retrying", "err", err, "backoff", backoff)
			t.emitStatus(core.TargetStatusDisconnected)
			if !sleepCtx(ctx, backoff) {
				return nil
			}
			backoff = min(backoff*2, reconnectMax)
			continue
		}
		backoff = reconnectBase
		t.log.Info("gateway port open")

		var sessErr error
		last, sessErr = t.runSession(ctx, port, last)
		if ctx.Err() != nil {
			return nil
		}
		t.log.Warn("gateway link lost, reopening", "err", sessErr, "backoff", backoff)
		t.emitStatus(core.TargetStatusDisconnected)
		if !sleepCtx(ctx, backoff) {
			return nil
		}
		backoff = min(backoff*2, reconnectMax)
	}
}

// runSession streams commands over one open port until it fails or ctx ends.
// It closes the port and waits for the reader before returning, and returns
// the last state written.
func (t *Target) runSession(ctx context.Context, port io.ReadWriteCloser, last core.InputCommand) (core.InputCommand, error) {
	readErr := make(chan error, 1)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		readErr <- t.readLoop(port)
	}()
	defer func() {
		_ = port.Close() // unblocks the reader
		<-readDone
	}()

	keep := time.NewTimer(0) // fires at once: re-send the held state after a reconnect
	defer keep.Stop()
	flush := time.NewTimer(time.Hour)
	flush.Stop()
	defer flush.Stop()

	write := func(cmd core.InputCommand, fresh bool) error {
		t.seq++
		if _, err := port.Write(EncodeInput(cmd, t.seq)); err != nil {
			return fmt.Errorf("write: %w", err)
		}
		if fresh && t.latency != nil && !cmd.Timestamp.IsZero() {
			t.latency.Observe(time.Since(cmd.Timestamp))
		}
		last = cmd
		// Only a held state needs refreshing. A neutral one is sent once:
		// the gateway neutralises on its own after its input timeout, and
		// re-sending it would only flip timeout_neutral and status reports.
		if isNeutral(cmd) {
			stopTimer(keep)
		} else {
			resetTimer(keep, t.keepalive)
		}
		return nil
	}
	drain := func() error {
		for {
			if cmd, ok := t.coalescer.Poll(); ok {
				if err := write(cmd, true); err != nil {
					return err
				}
				continue
			}
			if d := t.coalescer.UntilReady(); d >= 0 {
				if d == 0 {
					continue
				}
				resetTimer(flush, d)
			}
			return nil
		}
	}

	for {
		select {
		case <-ctx.Done():
			return last, nil
		case err := <-readErr:
			return last, fmt.Errorf("read: %w", err)
		case <-t.coalescer.NotifyC():
			if err := drain(); err != nil {
				return last, err
			}
		case <-flush.C:
			if err := drain(); err != nil {
				return last, err
			}
		case <-keep.C:
			if err := write(last, false); err != nil {
				return last, err
			}
		}
	}
}

func isNeutral(cmd core.InputCommand) bool {
	return cmd.Buttons == 0 && cmd.LX == 0 && cmd.LY == 0 && cmd.RX == 0 && cmd.RY == 0
}

// readLoop splits the byte stream on 0x00 delimiters and handles each frame
// until the port fails or is closed.
func (t *Target) readLoop(r io.Reader) error {
	var (
		buf     = make([]byte, 256)
		frame   []byte
		skip    bool // current frame overflowed; drop it at the delimiter
		mon     statusMonitor
		badSeen int
	)
	for {
		n, err := r.Read(buf)
		data := buf[:n]
		for len(data) > 0 {
			i := bytes.IndexByte(data, 0)
			if i < 0 {
				if !skip && len(frame)+len(data) <= maxEncodedFrame {
					frame = append(frame, data...)
				} else {
					skip = true
				}
				break
			}
			if !skip && len(frame)+i <= maxEncodedFrame && len(frame)+i > 0 {
				frame = append(frame, data[:i]...)
				if err := t.handleFrame(frame, &mon); err != nil {
					badSeen++
					if badSeen <= 3 {
						t.log.Debug("bad frame from gateway", "err", err)
					}
				}
			}
			frame, skip = frame[:0], false
			data = data[i+1:]
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return io.ErrUnexpectedEOF
			}
			return err
		}
	}
}

func (t *Target) handleFrame(enc []byte, mon *statusMonitor) error {
	typ, payload, err := DecodeFrame(enc)
	if err != nil {
		return err
	}
	if typ != typeStatus {
		return nil
	}
	s, err := ParseStatus(payload)
	if err != nil {
		return err
	}
	mon.update(t, s)
	return nil
}

// statusMonitor turns successive status reports into TargetStatus changes,
// apply-latency samples and warnings when error counters grow. It is used only
// by the reader goroutine.
type statusMonitor struct {
	prev Status
	have bool
}

func (m *statusMonitor) update(t *Target, s Status) {
	if !m.have || s.HIDState != m.prev.HIDState {
		t.log.Info("gateway status", "hid", hidStateName(s.HIDState), "source", s.ActiveSource)
		t.emitStatus(hidToTarget(s.HIDState))
	}
	if m.have {
		// The latency field only describes a new frame when the accepted
		// sequence moved; otherwise it repeats an old sample.
		if s.LastSeq != m.prev.LastSeq && s.ReceiveToSubmitUS > 0 && t.apply != nil {
			t.apply.Observe(time.Duration(s.ReceiveToSubmitUS) * time.Microsecond)
		}
		if d := counterDelta(s.Invalid, m.prev.Invalid) + counterDelta(s.Stale, m.prev.Stale) +
			counterDelta(s.Conflicts, m.prev.Conflicts); d > 0 {
			t.log.Warn("gateway rejected input",
				"invalid", s.Invalid, "stale", s.Stale, "conflicts", s.Conflicts)
		}
		if s.TimeoutNeutral() && !m.prev.TimeoutNeutral() {
			t.log.Debug("gateway neutralised after input timeout")
		}
	}
	m.prev, m.have = s, true
}

// counterDelta is how far a saturating u16 counter grew (0 if it reset).
func counterDelta(cur, prev uint16) int {
	if cur < prev {
		return 0
	}
	return int(cur - prev)
}

func hidToTarget(s uint8) core.TargetStatus {
	if s == HIDConnected {
		return core.TargetStatusConnected
	}
	return core.TargetStatusConnecting
}

func hidStateName(s uint8) string {
	switch s {
	case HIDIdle:
		return "idle"
	case HIDDiscoverable:
		return "discoverable"
	case HIDConnecting:
		return "connecting"
	case HIDConnected:
		return "connected"
	}
	return fmt.Sprintf("unknown(%d)", s)
}

// emitStatus publishes s without blocking; a full channel drops the update.
func (t *Target) emitStatus(s core.TargetStatus) {
	select {
	case t.statusCh <- s:
	default:
	}
}

func resetTimer(tm *time.Timer, d time.Duration) {
	stopTimer(tm)
	tm.Reset(d)
}

// stopTimer stops tm and drains a fire that already happened.
func stopTimer(tm *time.Timer) {
	if !tm.Stop() {
		select {
		case <-tm.C:
		default:
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-tm.C:
		return true
	}
}
