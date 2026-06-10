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

	start time.Time // stream start, for PTS computation
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
	}, nil
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
// closes, or the subprocess exits. A subprocess failure is reported on Errors()
// and ends the module cleanly (returns nil) so the supervisor — not the
// errgroup — decides whether to restart.
func (e *Encoder) Run(ctx context.Context) error {
	defer close(e.packets)
	defer close(e.errs)
	defer e.log.Info("encoder stopped")

	args, err := BuildArgs(e.opts)
	if err != nil {
		return fmt.Errorf("encoder: build args: %w", err)
	}

	// A child context so we can stop the writer/reader without depending on the
	// parent being cancelled (e.g. when ffmpeg dies on its own).
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(runCtx, e.opts.FFmpegPath, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("encoder: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("encoder: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("encoder: stderr pipe: %w", err)
	}

	e.log.Info("starting ffmpeg", "path", e.opts.FFmpegPath, "args", args)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("encoder: start ffmpeg: %w", err)
	}
	e.start = time.Now()

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
		return nil

	case err := <-waitErr:
		// ffmpeg exited on its own. This is unexpected during normal operation.
		cancel()
		wg.Wait()
		rerr := <-readErr
		e.reportSubprocessExit(ctx, err, rerr)
		return nil
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
	pkt := core.EncodedPacket{
		Data:       au.data,
		PTS:        time.Since(e.start),
		IsKeyframe: au.isKeyframe,
	}
	select {
	case e.packets <- pkt:
		return
	default:
	}
	// Channel full: drop the oldest queued packet, then enqueue the new one.
	select {
	case <-e.packets:
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
