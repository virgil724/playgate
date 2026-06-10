// Command host is the PlayGate Host entrypoint. It loads configuration, wires
// the pipeline modules together, and runs them until interrupted (Ctrl+C /
// SIGTERM), at which point every module shuts down gracefully.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/playgate/playgate-host/internal/config"
	"github.com/playgate/playgate-host/internal/host"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to the YAML configuration file")
	debug := flag.Bool("debug", false, "enable debug-level logging")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Error("failed to load config", "err", err)
		os.Exit(1)
	}
	log.Info("config loaded", "path", *configPath, "room", cfg.Signaling.RoomID)

	// NotifyContext cancels ctx on the first SIGINT/SIGTERM, giving modules a
	// chance to shut down cleanly. A second signal will terminate the process.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	h, err := host.New(log, cfg)
	if err != nil {
		log.Error("failed to build host", "err", err)
		os.Exit(1)
	}
	if err := h.Run(ctx); err != nil {
		log.Error("host exited with error", "err", err)
		os.Exit(1)
	}
}
