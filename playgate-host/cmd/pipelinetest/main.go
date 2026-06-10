// Command pipelinetest is a Linux-only verification harness for the capture →
// encode pipeline (PlayGate T2+T3). It opens a V4L2 capture device, negotiates a
// pixel format, runs the frames through the ffmpeg H.264 encoder, writes the
// resulting Annex-B elementary stream to a .h264 file, and saves the first raw
// frame as a snapshot for sanity-checking the capture path.
//
// It cannot run on the dev machine (no V4L2, no ffmpeg) but compiles on every
// platform so cross-builds stay green. On non-Linux it exits with a message.
//
// Example (on the target box):
//
//	go run ./cmd/pipelinetest -device /dev/video0 -w 1280 -h 720 -fps 30 \
//	    -seconds 5 -out capture.h264 -snapshot frame0.raw
//
// The .h264 file can be played with:  ffplay capture.h264
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/playgate/playgate-host/internal/capture/v4l2"
	"github.com/playgate/playgate-host/internal/core"
	"github.com/playgate/playgate-host/internal/encoder/ffmpeg"
)

func main() {
	device := flag.String("device", "/dev/video0", "V4L2 capture device path")
	width := flag.Int("w", 1280, "capture width")
	height := flag.Int("h", 720, "capture height")
	fps := flag.Int("fps", 30, "capture frame rate")
	seconds := flag.Int("seconds", 5, "how long to capture before stopping (0 = until Ctrl+C)")
	out := flag.String("out", "capture.h264", "output Annex-B H.264 file")
	snapshot := flag.String("snapshot", "", "optional path to save the first raw frame")
	ffmpegPath := flag.String("ffmpeg", "ffmpeg", "ffmpeg binary path")
	bitrate := flag.Int("bitrate", 6_000_000, "H.264 target bitrate (bits/sec)")
	debug := flag.Bool("debug", false, "enable debug logging")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *seconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(*seconds)*time.Second)
		defer cancel()
	}

	if err := run(ctx, log, pipelineConfig{
		device:     *device,
		width:      *width,
		height:     *height,
		fps:        *fps,
		out:        *out,
		snapshot:   *snapshot,
		ffmpegPath: *ffmpegPath,
		bitrate:    *bitrate,
	}); err != nil {
		log.Error("pipeline failed", "err", err)
		os.Exit(1)
	}
}

type pipelineConfig struct {
	device     string
	width      int
	height     int
	fps        int
	out        string
	snapshot   string
	ffmpegPath string
	bitrate    int
}

func run(ctx context.Context, log *slog.Logger, cfg pipelineConfig) error {
	// 1. Capture source.
	capCfg := v4l2.Config{
		Device: cfg.device,
		Width:  cfg.width,
		Height: cfg.height,
		FPS:    cfg.fps,
	}
	src, err := v4l2.New(log, capCfg)
	if err != nil {
		return fmt.Errorf("create capture: %w", err)
	}
	if err := src.Start(ctx); err != nil {
		return fmt.Errorf("start capture: %w", err)
	}

	// Tap the frame channel: save the first frame as a snapshot, forward the rest.
	encIn := make(chan core.VideoFrame, 4)
	tap := newSnapshotTap(log, src.Frames(), encIn, cfg.snapshot)

	// 2. Encoder.
	encOpts := ffmpeg.DefaultOptions(cfg.width, cfg.height, cfg.fps, core.PixelFormatYUYV)
	encOpts.FFmpegPath = cfg.ffmpegPath
	encOpts.Bitrate = cfg.bitrate
	enc, err := ffmpeg.New(log, encOpts, encIn)
	if err != nil {
		return fmt.Errorf("create encoder: %w", err)
	}

	// 3. Sink: write encoded packets to the .h264 file.
	f, err := os.Create(cfg.out)
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	defer f.Close()

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return src.Run(gctx) })
	g.Go(func() error { return tap.run(gctx) })
	g.Go(func() error { return enc.Run(gctx) })
	g.Go(func() error { return writePackets(gctx, log, enc.Packets(), f) })
	g.Go(func() error { return logErrors(gctx, log, "capture", src.Errors()) })
	g.Go(func() error { return logErrors(gctx, log, "encoder", enc.Errors()) })

	if err := g.Wait(); err != nil && ctx.Err() == nil {
		return err
	}
	log.Info("pipeline finished", "output", cfg.out)
	return nil
}

// snapshotTap forwards frames from in to out, saving the first frame's raw bytes
// to a file (if a path is set). It closes out when in closes.
type snapshotTap struct {
	log      *slog.Logger
	in       <-chan core.VideoFrame
	out      chan<- core.VideoFrame
	path     string
	saved    bool
}

func newSnapshotTap(log *slog.Logger, in <-chan core.VideoFrame, out chan<- core.VideoFrame, path string) *snapshotTap {
	return &snapshotTap{log: log, in: in, out: out, path: path}
}

func (t *snapshotTap) run(ctx context.Context) error {
	defer close(t.out)
	for {
		select {
		case <-ctx.Done():
			return nil
		case frame, ok := <-t.in:
			if !ok {
				return nil
			}
			if !t.saved && t.path != "" {
				if err := os.WriteFile(t.path, frame.Data, 0o644); err != nil {
					t.log.Warn("failed to save snapshot", "err", err)
				} else {
					t.log.Info("saved snapshot", "path", t.path, "bytes", len(frame.Data))
				}
				t.saved = true
			}
			select {
			case t.out <- frame:
			case <-ctx.Done():
				return nil
			}
		}
	}
}

// writePackets drains encoded packets to w until the channel closes.
func writePackets(ctx context.Context, log *slog.Logger, packets <-chan core.EncodedPacket, w *os.File) error {
	var (
		count int
		bytes int
	)
	defer func() { log.Info("wrote encoded stream", "packets", count, "bytes", bytes) }()
	for {
		select {
		case <-ctx.Done():
			return nil
		case pkt, ok := <-packets:
			if !ok {
				return nil
			}
			n, err := w.Write(pkt.Data)
			if err != nil {
				return fmt.Errorf("write packet: %w", err)
			}
			count++
			bytes += n
		}
	}
}

// logErrors logs any non-fatal errors emitted by a module until its channel
// closes. It never returns an error itself (these are informational).
func logErrors(ctx context.Context, log *slog.Logger, module string, errs <-chan error) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case err, ok := <-errs:
			if !ok {
				return nil
			}
			if err != nil {
				log.Warn("module error", "module", module, "err", err)
			}
		}
	}
}
