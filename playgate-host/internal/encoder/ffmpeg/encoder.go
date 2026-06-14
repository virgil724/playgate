package ffmpeg

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"github.com/playgate/playgate-host/internal/core"
)

// readChunkSize is the stdout read granularity from ffmpeg. A few KB balances
// syscall overhead against latency.
const readChunkSize = 32 * 1024

// packetBufferSize bounds the encoded-packet output channel. Encoded H.264 AUs
// are small and the downstream (WebRTC) should keep up; a tiny buffer keeps
// latency low while absorbing brief jitter.
const packetBufferSize = 4

// Encoder is a core H.264 encoder that pipes raw VideoFrames through an ffmpeg
// subprocess and emits Annex-B EncodedPackets. It implements core.Module.
//
// Ownership: the Encoder owns the packets and errs channels — Run is the sole
// writer and closes them on exit, per the core channel contract. It consumes
// the upstream VideoFrame channel (read-only).
//
// Backpressure: frames are written to ffmpeg's stdin from a dedicated writer
// goroutine fed by an internal bounded queue. When ffmpeg falls behind, the
// newest frame replaces the oldest queued frame (drop-oldest) so the capture
// stage never blocks. Encoded packets use the same drop-oldest discipline.
type Encoder struct {
	log  *slog.Logger
	opts Options
	in   <-chan core.VideoFrame

	packets chan core.EncodedPacket
	errs    chan error

	// framesOut counts access units emitted across the encoder's whole life
	// (spanning ffmpeg restarts). PTS is derived from this counter and the target
	// FPS, NOT from wall-clock time: ffmpeg flushes frames to stdout in bursts, so
	// wall-clock read-times bunch several frames into nearly-identical PTS, which
	// collapses to duplicate RTP timestamps and makes a browser jitter buffer drop
	// every frame but the keyframes. A counter gives evenly-spaced, monotonic PTS.
	framesOut int64

	// restart signals the Run loop to tear down the current ffmpeg subprocess and
	// relaunch it with the latest options (used by SetBitrate). Buffered size 1
	// with coalescing so rapid requests collapse into one restart.
	restart chan struct{}

	// mu guards bitrate and lastForce (the fields mutated after construction). The
	// encoder args are rebuilt from opts on each (re)launch under this lock.
	mu sync.Mutex
	// lastForce is when ForceKeyframe last triggered a restart, used to debounce a
	// burst of viewer PLIs into at most one restart per forceKeyframeDebounce.
	lastForce time.Time
}

// Ensure Encoder satisfies core.Module at compile time.
var _ core.Module = (*Encoder)(nil)

// New constructs an ffmpeg-backed Encoder reading frames from in. Options are
// validated eagerly so misconfiguration surfaces before Run.
func New(log *slog.Logger, opts Options, in <-chan core.VideoFrame) (*Encoder, error) {
	o := opts.normalise()
	if err := o.Validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	return &Encoder{
		log:     log.With("module", "encoder", "codec", o.Codec.Name()),
		opts:    o,
		in:      in,
		packets: make(chan core.EncodedPacket, packetBufferSize),
		errs:    make(chan error, 1),
		restart: make(chan struct{}, 1),
	}, nil
}

// SetBitrate updates the target bitrate (bits per second) and, if it differs
// from the current value, requests a subprocess restart so the new rate takes
// effect. ffmpeg cannot change an encoder's bitrate on a running subprocess, so
// the bitrate-apply strategy is: rebuild args and relaunch ffmpeg. The fresh
// subprocess emits an IDR immediately, and the rtc layer's keyframe gating drops
// any inter frames until that IDR arrives, so the restart does not show as
// corruption on the viewer. Restart frequency is bounded upstream by the ABR
// controller's cooldown. SetBitrate is safe for concurrent use and non-blocking.
func (e *Encoder) SetBitrate(bps int) {
	if bps <= 0 {
		return
	}
	e.mu.Lock()
	changed := e.opts.Bitrate != bps
	if changed {
		e.opts.Bitrate = bps
	}
	e.mu.Unlock()
	if !changed {
		return
	}
	e.log.Info("bitrate change requested; will restart encoder", "bitrate", bps)
	// Coalesce: a full channel already has a pending restart that will pick up the
	// latest opts.Bitrate when it fires.
	select {
	case e.restart <- struct{}{}:
	default:
	}
}

// forceKeyframeDebounce bounds how often ForceKeyframe may restart ffmpeg, so a
// burst of viewer PLIs (which arrive once per lost/garbled frame) cannot turn
// into a restart loop. One second matches the default GOP, so a forced IDR is
// never more than ~1 GOP earlier than the scheduled one anyway.
const forceKeyframeDebounce = time.Second

// ForceKeyframe asks the encoder to emit an IDR as soon as possible. ffmpeg
// cannot inject an on-demand IDR into a running subprocess, so — like SetBitrate
// — this restarts the subprocess (the relaunched ffmpeg emits an IDR
// immediately; the rtc layer's keyframe gating hides the brief gap). It is the
// host's response to a viewer Picture Loss Indication. Calls are debounced by
// forceKeyframeDebounce and coalesced onto the single restart slot, so a PLI
// storm costs at most one restart per second. Safe for concurrent use and
// non-blocking.
func (e *Encoder) ForceKeyframe() {
	e.mu.Lock()
	now := time.Now()
	if !e.lastForce.IsZero() && now.Sub(e.lastForce) < forceKeyframeDebounce {
		e.mu.Unlock()
		return
	}
	e.lastForce = now
	e.mu.Unlock()
	e.log.Info("keyframe requested (PLI); will restart encoder for an IDR")
	select {
	case e.restart <- struct{}{}:
	default:
	}
}

// currentArgs builds the ffmpeg arg vector from the current options under the
// lock so a concurrent SetBitrate cannot tear the read.
func (e *Encoder) currentArgs() ([]string, error) {
	e.mu.Lock()
	opts := e.opts
	e.mu.Unlock()
	return BuildArgs(opts)
}

// Name implements core.Module.
func (e *Encoder) Name() string { return "encoder" }

// Packets returns the receive-only channel of encoded H.264 packets. The
// Encoder owns and closes it on exit.
func (e *Encoder) Packets() <-chan core.EncodedPacket { return e.packets }

// Errors returns the receive-only channel of non-fatal encoder errors (e.g. the
// ffmpeg subprocess died). The upstream supervisor may decide to restart the
// module on receipt. The Encoder owns and closes it on exit.
func (e *Encoder) Errors() <-chan error { return e.errs }

// Run implements core.Module. It launches ffmpeg, pumps frames in and packets
// out, and tears everything down when the context is cancelled, the input
// closes, or the subprocess exits.
//
// Run is a supervision loop around a single ffmpeg subprocess: a SetBitrate call
// requests a restart, on which Run cleanly stops the current subprocess and
// relaunches it with the new bitrate. The module-lifetime packets/errs channels
// are owned by Run and closed once when it returns (the per-subprocess pipes are
// distinct and closed each iteration). A subprocess that exits on its own (not a
// restart) is reported on Errors() and ends the module cleanly (returns nil) so
// the supervisor — not the errgroup — decides whether to restart.
func (e *Encoder) Run(ctx context.Context) error {
	defer close(e.packets)
	defer close(e.errs)
	defer e.log.Info("encoder stopped")

	for {
		restartRequested, err := e.runSubprocess(ctx)
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		if !restartRequested {
			// ffmpeg exited on its own (already reported); end the module.
			return nil
		}
		e.log.Info("restarting ffmpeg with updated options")
	}
}

// runSubprocess launches one ffmpeg subprocess and pumps frames until ctx is
// cancelled, a restart is requested (SetBitrate), or the subprocess exits. It
// returns restartRequested=true only when a restart was explicitly requested;
// any setup error is returned as a fatal err (ends the module).
func (e *Encoder) runSubprocess(ctx context.Context) (restartRequested bool, err error) {
	args, err := e.currentArgs()
	if err != nil {
		return false, fmt.Errorf("encoder: build args: %w", err)
	}

	// A child context so we can stop the writer/reader without depending on the
	// parent being cancelled (e.g. when ffmpeg dies on its own or we restart).
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(runCtx, e.opts.FFmpegPath, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return false, fmt.Errorf("encoder: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, fmt.Errorf("encoder: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return false, fmt.Errorf("encoder: stderr pipe: %w", err)
	}

	e.log.Info("starting ffmpeg", "path", e.opts.FFmpegPath, "args", args)
	if err := cmd.Start(); err != nil {
		return false, fmt.Errorf("encoder: start ffmpeg: %w", err)
	}

	var wg sync.WaitGroup
	wg.Add(3)

	// Drain stderr so ffmpeg never blocks on a full stderr pipe; log lines.
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			e.log.Warn("ffmpeg", "msg", sc.Text())
		}
	}()

	// Writer: feed frames into ffmpeg stdin with drop-oldest backpressure.
	go func() {
		defer wg.Done()
		e.writeFrames(runCtx, stdin)
	}()

	// Reader: split stdout into access units and emit packets.
	readErr := make(chan error, 1)
	go func() {
		defer wg.Done()
		readErr <- e.readPackets(runCtx, stdout)
	}()

	// Wait for ffmpeg to exit or the context to end.
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		// Clean shutdown requested. Cancel the child context, which kills ffmpeg.
		cancel()
		<-waitErr
		wg.Wait()
		return false, nil

	case <-e.restart:
		// Bitrate change: stop this subprocess, then loop to relaunch with new args.
		e.log.Info("stopping ffmpeg for restart")
		cancel()
		<-waitErr
		wg.Wait()
		return true, nil

	case werr := <-waitErr:
		// ffmpeg exited on its own. This is unexpected during normal operation.
		cancel()
		wg.Wait()
		rerr := <-readErr
		e.reportSubprocessExit(ctx, werr, rerr)
		return false, nil
	}
}

// frameQueueSize bounds the encoder's internal frame queue that sits between
// the input channel reader and the ffmpeg stdin writer. When ffmpeg stalls the
// writer stops draining this queue; the reader then drops the oldest queued
// frame rather than blocking the upstream capture stage. A small queue keeps
// latency low.
const frameQueueSize = 2

// writeFrames consumes the input frame channel and writes raw frame bytes to
// ffmpeg's stdin. It decouples reading from writing with an internal bounded
// queue so a stalled ffmpeg never blocks the upstream capture stage: when the
// queue is full the oldest queued frame is dropped (drop-oldest backpressure).
//
// Two goroutines cooperate: this one reads frames from e.in and pushes them onto
// the queue (dropping the oldest on overflow); a writer goroutine pulls from the
// queue and writes to stdin. The writer owns closing stdin so ffmpeg sees EOF.
func (e *Encoder) writeFrames(ctx context.Context, stdin io.WriteCloser) {
	queue := make(chan core.VideoFrame, frameQueueSize)

	// Writer goroutine: drains the queue to ffmpeg stdin.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer stdin.Close() // EOF tells ffmpeg to flush and exit
		for {
			select {
			case <-ctx.Done():
				return
			case frame, ok := <-queue:
				if !ok {
					return // reader closed the queue: end of stream
				}
				if _, err := stdin.Write(frame.Data); err != nil {
					// Broken pipe: ffmpeg is gone. Run's waitErr path reports it.
					if !errors.Is(err, io.ErrClosedPipe) {
						e.log.Debug("stdin write failed", "err", err)
					}
					return
				}
			}
		}
	}()

	// Reader loop: pull frames from upstream and enqueue with drop-oldest.
	defer func() {
		close(queue)
		<-writerDone
	}()
	frameSizeLogged := false
	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-e.in:
			if !ok {
				return // upstream closed: end of stream
			}
			if frame.PixelFormat != e.opts.InputFormat {
				// Format mismatch would corrupt the rawvideo stream; skip it.
				e.log.Warn("dropping frame: format mismatch",
					"got", frame.PixelFormat, "want", e.opts.InputFormat)
				continue
			}
			if frame.Width != e.opts.Width || frame.Height != e.opts.Height {
				// Resolution mismatch would corrupt the rawvideo stream; skip it.
				e.log.Warn("dropping frame: resolution mismatch",
					"got", fmt.Sprintf("%dx%d", frame.Width, frame.Height),
					"want", fmt.Sprintf("%dx%d", e.opts.Width, e.opts.Height))
				continue
			}
			if !frameSizeLogged {
				frameSizeLogged = true
				expected := expectedRawSize(e.opts.Width, e.opts.Height, e.opts.InputFormat)
				e.log.Info("first frame diagnostic",
					"data_len", len(frame.Data),
					"expected_len", expected,
					"width", frame.Width, "height", frame.Height,
					"format", frame.PixelFormat)
			}
			e.enqueueFrame(queue, frame)
		}
	}
}

// enqueueFrame pushes a frame onto the writer queue without blocking. If the
// queue is full it drops the oldest queued frame and retries once, so a stalled
// ffmpeg cannot back-pressure the upstream capture stage.
func (e *Encoder) enqueueFrame(queue chan core.VideoFrame, frame core.VideoFrame) {
	select {
	case queue <- frame:
		return
	default:
	}
	// Full: drop the oldest queued frame, then enqueue the new one.
	select {
	case <-queue:
		e.log.Debug("dropped frame: ffmpeg not keeping up")
	default:
	}
	select {
	case queue <- frame:
	default:
		// Still full (writer raced us): drop the new frame too rather than block.
		e.log.Debug("dropped frame: writer queue contended")
	}
}

// readPackets reads the Annex-B stream from ffmpeg's stdout, splits it into
// access units, and forwards each as a core.EncodedPacket with drop-oldest
// backpressure on the output channel. It returns the first non-EOF read error.
func (e *Encoder) readPackets(ctx context.Context, stdout io.Reader) error {
	splitter := NewAnnexBSplitter()
	buf := make([]byte, readChunkSize)
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			for _, au := range splitter.Push(buf[:n]) {
				e.emitPacket(ctx, au)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				for _, au := range splitter.Flush() {
					e.emitPacket(ctx, au)
				}
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

// emitPacket converts an access unit to a core.EncodedPacket and pushes it onto
// the output channel using drop-oldest semantics so a slow consumer never
// blocks the encoder.
func (e *Encoder) emitPacket(ctx context.Context, au accessUnit) {
	fps := e.opts.FPS
	if fps <= 0 {
		fps = 30
	}
	// Evenly-spaced monotonic PTS from the emitted-frame counter (see framesOut).
	pts := time.Duration(e.framesOut) * time.Second / time.Duration(fps)
	e.framesOut++
	pkt := core.EncodedPacket{
		Data:       au.data,
		PTS:        pts,
		IsKeyframe: au.isKeyframe,
	}
	select {
	case e.packets <- pkt:
		return
	default:
	}
	// Channel full: drop the oldest queued packet to make room for the newer one,
	// favouring fresh frames so the viewer's fps holds up under a slow consumer.
	// A keyframe must never be dropped: a 1080p IDR spans hundreds of RTP packets
	// and losing it freezes the decoder (green/stuck frame) until the next GOP —
	// the exact opposite of smooth playback. If the oldest queued packet is a
	// keyframe, put it back and drop the incoming inter-frame instead.
	select {
	case old := <-e.packets:
		if old.IsKeyframe {
			select {
			case e.packets <- old:
			default:
			}
			e.log.Debug("dropped encoded packet to preserve queued keyframe")
			return
		}
	default:
	}
	select {
	case e.packets <- pkt:
	case <-ctx.Done():
	default:
		e.log.Debug("dropped encoded packet: consumer not keeping up")
	}
}

// reportSubprocessExit emits an error describing why ffmpeg stopped. It is
// best-effort and non-blocking.
func (e *Encoder) reportSubprocessExit(ctx context.Context, waitErr, readErr error) {
	var msg error
	switch {
	case waitErr != nil:
		msg = fmt.Errorf("encoder: ffmpeg exited: %w", waitErr)
	case readErr != nil:
		msg = fmt.Errorf("encoder: ffmpeg stdout error: %w", readErr)
	default:
		msg = errors.New("encoder: ffmpeg exited unexpectedly")
	}
	e.log.Warn("ffmpeg subprocess ended", "err", msg)
	select {
	case e.errs <- msg:
	case <-ctx.Done():
	default:
	}
}

// expectedRawSize returns the expected byte count for a single raw frame at the
// given dimensions and format. Returns -1 for variable-length formats (MJPEG).
func expectedRawSize(w, h int, fmt core.PixelFormat) int {
	switch fmt {
	case core.PixelFormatNV12:
		return w * h * 3 / 2
	case core.PixelFormatYUYV:
		return w * h * 2
	default:
		return -1
	}
}
