// Command sunshine-agent runs the PlayGate Sunshine management agent (T15).
//
// In PC mode PlayGate does not handle video or input; it acts as a management
// layer on top of Sunshine/Moonlight.  This binary:
//
//  1. Reads a PlayGate session JWT from a flag or stdin.
//  2. Creates a session.Manager that validates and times the session.
//  3. Starts the sunshine.Agent which watches session events and calls
//     the Sunshine REST API to approve pairing and kick clients on expiry.
//
// # Quick start
//
//	sunshine-agent \
//	  -sunshine-url   https://localhost:47990 \
//	  -sunshine-user  admin \
//	  -sunshine-pass  s3cr3t \
//	  -pubkey         <base64-ed25519-pubkey> \
//	  -room           my-room \
//	  -token          <JWT>
//
// See docs/sunshine.md for full setup instructions.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/playgate/playgate-host/internal/session"
	"github.com/playgate/playgate-host/internal/sunshine"
)

// agentCLIConfig is the configuration collected from flags / environment.
type agentCLIConfig struct {
	// Sunshine API settings.
	sunshineURL      string
	sunshineUser     string
	sunshinePass     string
	sunshineInsecure bool

	// PlayGate session settings.
	pubKeyBase64 string
	roomID       string
	idleTimeout  time.Duration

	// Token for the initial session claim.
	// If empty the agent waits for a token on stdin (one per line).
	token string

	// Pairing PIN to automatically approve when a session is granted.
	// Leave empty to skip auto-approval (operator approves manually in
	// Moonlight / Sunshine web UI).
	pairPIN string

	// Logging.
	debug bool
}

func main() {
	cfg := parseFlags()

	level := slog.LevelInfo
	if cfg.debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if err := run(cfg, log); err != nil {
		log.Error("sunshine-agent exited with error", "err", err)
		os.Exit(1)
	}
}

func parseFlags() agentCLIConfig {
	var cfg agentCLIConfig

	flag.StringVar(&cfg.sunshineURL, "sunshine-url", "https://localhost:47990",
		"Base URL of the Sunshine REST API")
	flag.StringVar(&cfg.sunshineUser, "sunshine-user", "",
		"Sunshine web-UI username (basic auth)")
	flag.StringVar(&cfg.sunshinePass, "sunshine-pass", "",
		"Sunshine web-UI password (basic auth)")
	flag.BoolVar(&cfg.sunshineInsecure, "sunshine-insecure", true,
		"Skip TLS certificate verification (Sunshine uses a self-signed cert by default)")
	flag.StringVar(&cfg.pubKeyBase64, "pubkey", "",
		"Base64-encoded ed25519 public key used to verify session JWTs (required)")
	flag.StringVar(&cfg.roomID, "room", "",
		"Room ID that JWTs must match (required)")
	flag.DurationVar(&cfg.idleTimeout, "idle-timeout", 0,
		"Kick viewer after this duration of no Moonlight input (0 = disabled)")
	flag.StringVar(&cfg.token, "token", "",
		"Session JWT to claim immediately.  If empty, read one token per line from stdin.")
	flag.StringVar(&cfg.pairPIN, "pair-pin", "",
		"4-digit Moonlight pairing PIN to approve automatically on session grant.  "+
			"Leave empty to skip auto-approval.")
	flag.BoolVar(&cfg.debug, "debug", false, "Enable debug-level logging")

	flag.Parse()

	if cfg.pubKeyBase64 == "" {
		fmt.Fprintln(os.Stderr, "ERROR: -pubkey is required")
		flag.Usage()
		os.Exit(1)
	}
	if cfg.roomID == "" {
		fmt.Fprintln(os.Stderr, "ERROR: -room is required")
		flag.Usage()
		os.Exit(1)
	}

	return cfg
}

func run(cfg agentCLIConfig, log *slog.Logger) error {
	// Build the Sunshine HTTP client.
	ctrl := sunshine.NewHTTPClient(sunshine.ClientConfig{
		BaseURL:            cfg.sunshineURL,
		Username:           cfg.sunshineUser,
		Password:           cfg.sunshinePass,
		InsecureSkipVerify: cfg.sunshineInsecure,
		Timeout:            10 * time.Second,
		MaxRetries:         3,
		RetryDelay:         500 * time.Millisecond,
	})

	// Probe Sunshine to give an early error if it is not reachable.
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer probeCancel()
	info, err := ctrl.Status(probeCtx)
	if err != nil {
		return fmt.Errorf("cannot reach Sunshine at %s: %w", cfg.sunshineURL, err)
	}
	log.Info("Sunshine connected", "version", info.Version, "platform", info.Platform)

	// Build the session manager.
	mgr, err := session.NewManager(session.Config{
		PublicKeyBase64: cfg.pubKeyBase64,
		RoomID:          cfg.roomID,
		IdleTimeout:     cfg.idleTimeout,
		TickInterval:    5 * time.Second,
		QueuePolicy:     session.PolicyReject, // PC mode: only one viewer at a time
	})
	if err != nil {
		return fmt.Errorf("create session manager: %w", err)
	}

	// Set up top-level context that is cancelled on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Start the session manager.
	go func() {
		if err := mgr.Run(ctx); err != nil {
			log.Error("session manager error", "err", err)
		}
	}()

	// Build and start the Sunshine agent.
	ag, err := sunshine.NewAgent(sunshine.AgentConfig{
		Controller:     ctrl,
		Events:         mgr.Events(),
		PairPIN:        cfg.pairPIN,
		KickRetries:    3,
		KickRetryDelay: time.Second,
		Log:            log,
	})
	if err != nil {
		return fmt.Errorf("create sunshine agent: %w", err)
	}
	go func() {
		if err := ag.Run(ctx); err != nil {
			log.Error("sunshine agent error", "err", err)
		}
	}()

	// Claim session(s).
	if cfg.token != "" {
		if err := claimToken(mgr, cfg.token, log); err != nil {
			log.Warn("initial token claim failed", "err", err)
		}
	} else {
		// Read tokens from stdin, one per line.  This allows a parent process
		// (e.g. the playgate-server) to pipe tokens in as viewers connect.
		//
		// TODO: in production integrate directly with playgate-server's token
		// issuance instead of reading from stdin.
		go readTokensFromStdin(ctx, mgr, log)
	}

	// Wait for shutdown signal.
	<-ctx.Done()
	log.Info("sunshine-agent shutting down")
	return nil
}

// claimToken attempts to claim a session with the given JWT token.
func claimToken(mgr *session.Manager, token string, log *slog.Logger) error {
	sess, err := mgr.Claim(token)
	if err != nil {
		return fmt.Errorf("claim token: %w", err)
	}
	log.Info("session claimed",
		"viewer", sess.ViewerID(),
		"expires_in", fmt.Sprintf("%ds", sess.Claims().SessionSeconds))
	return nil
}

// readTokensFromStdin reads one JWT per line from stdin and claims each.
// It exits when ctx is cancelled or stdin is closed.
func readTokensFromStdin(ctx context.Context, mgr *session.Manager, log *slog.Logger) {
	scanner := bufio.NewScanner(os.Stdin)
	for {
		// Check for cancellation before each blocking read.
		select {
		case <-ctx.Done():
			return
		default:
		}

		if !scanner.Scan() {
			// EOF or error.
			if err := scanner.Err(); err != nil {
				log.Error("stdin read error", "err", err)
			}
			return
		}

		token := strings.TrimSpace(scanner.Text())
		if token == "" || strings.HasPrefix(token, "#") {
			continue
		}

		if err := claimToken(mgr, token, log); err != nil {
			log.Warn("token claim failed", "err", err)
		}
	}
}
