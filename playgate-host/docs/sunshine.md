# PlayGate PC Mode — Sunshine Agent

## Overview

In **PC mode** PlayGate does **not** re-encode or relay video.  Instead it
acts as a thin management layer on top of
[Sunshine](https://app.lizardbyte.dev/Sunshine/) (the open-source GameStream
server) and [Moonlight](https://moonlight-stream.org/) (the client).

```
┌────────────────────────────────────────────────────────────┐
│  Host PC                                                   │
│  ┌──────────────┐  REST API   ┌──────────────────────────┐ │
│  │ sunshine-    │ ──────────▶ │ Sunshine (port 47990)    │ │
│  │ agent        │             │  · encodes & streams     │ │
│  │  (T15)       │             │  · handles Moonlight     │ │
│  └──────┬───────┘             └────────────┬─────────────┘ │
│         │ session events                   │ RTSP/video    │
│  ┌──────▼───────┐                          │               │
│  │ session.     │                          │               │
│  │ Manager      │                   ┌──────▼──────┐        │
│  └──────────────┘                   │  Moonlight  │        │
│                                     │  (viewer)   │        │
└─────────────────────────────────────┴─────────────┘        │
```

The agent watches `session.Manager` events:

| Event         | Agent action                                       |
|---------------|----------------------------------------------------|
| `granted`     | (optional) call `POST /api/pin` to approve pairing |
| `expired`     | call `POST /api/apps/close` to kick all clients    |
| `idle_kicked` | same as expired                                    |
| `queued`      | log only                                           |
| `tick`        | log only                                           |

## Sunshine Setup

### 1. Install Sunshine

Download the latest release from
<https://github.com/LizardByte/Sunshine/releases>.

Supported platforms: Windows, Linux (X11/Wayland), macOS.

After installing, the Sunshine web UI is available at
`https://localhost:47990` (self-signed TLS certificate).

### 2. Enable the REST API

The REST API is enabled by default.  Confirm in the web UI under
**Configuration → General** that the web UI port is `47990`.

### 3. Create a Sunshine account

Open `https://localhost:47990` in a browser, accept the TLS warning, and
set a username and password.  These are the credentials passed to
`sunshine-agent` via `-sunshine-user` / `-sunshine-pass`.

### 4. Obtain the Ed25519 public key

The agent validates PlayGate session JWTs.  Get the base64-encoded public key
from your `playgate-server` deployment:

```sh
playgate-server print-pubkey
```

### 5. Run the agent

```sh
sunshine-agent \
  -sunshine-url    https://localhost:47990 \
  -sunshine-user   admin \
  -sunshine-pass   s3cr3t \
  -sunshine-insecure          \   # Sunshine uses a self-signed cert by default
  -pubkey          <base64-ed25519-pubkey> \
  -room            my-room \
  -token           <JWT>
```

Alternatively, pipe one token per line on stdin (for server-driven flows):

```sh
echo "$JWT" | sunshine-agent -pubkey <key> -room my-room \
  -sunshine-url https://localhost:47990 \
  -sunshine-user admin -sunshine-pass s3cr3t
```

### Available flags

| Flag                   | Default                    | Description                                     |
|------------------------|----------------------------|-------------------------------------------------|
| `-sunshine-url`        | `https://localhost:47990`  | Sunshine REST API base URL                      |
| `-sunshine-user`       | _(empty)_                  | Sunshine web-UI username                        |
| `-sunshine-pass`       | _(empty)_                  | Sunshine web-UI password                        |
| `-sunshine-insecure`   | `true`                     | Skip TLS cert verification (self-signed OK)     |
| `-pubkey`              | _(required)_               | Base64 ed25519 public key for JWT verification  |
| `-room`                | _(required)_               | Room ID that JWTs must contain                  |
| `-idle-timeout`        | `0` (disabled)             | Kick viewer after N seconds of no Moonlight input|
| `-token`               | _(empty → read stdin)_     | JWT to claim immediately                        |
| `-pair-pin`            | _(empty → skip)_           | Moonlight pairing PIN for auto-approval         |
| `-debug`               | `false`                    | Enable debug logging                            |

## Sunshine REST API Endpoint Table

The endpoints below are derived from Sunshine v0.23.x source code.
**They may need adjustment for other Sunshine versions.**  Override paths via
`ClientConfig.Override*` fields when embedding the client programmatically.

| Constant              | Method | Path                 | Purpose                                   |
|-----------------------|--------|----------------------|-------------------------------------------|
| `endpointStatus`      | GET    | `/api/status`        | Probe Sunshine, get version/platform      |
| `endpointClients`     | GET    | `/api/clients`       | List paired Moonlight clients             |
| `endpointAppsList`    | GET    | `/api/apps`          | List Sunshine app entries                 |
| `endpointPairPinApprove` | POST | `/api/pin`          | Submit Moonlight pairing PIN              |
| `endpointCloseApp`    | POST   | `/api/apps/close`    | Kick all clients / close active session   |
| `endpointQuit`        | POST   | `/api/quit`          | Gracefully shut down Sunshine             |

> **Note for older Sunshine builds (< v0.19):** the pin endpoint was `/pin`
> (no `/api` prefix).  Set `ClientConfig.OverridePairPin = "/pin"` if needed.

## Viewer Experience Trade-offs

### Option A: Moonlight client (recommended)

The viewer installs the [Moonlight](https://moonlight-stream.org/) app on
their device (Windows, macOS, Linux, Android, iOS, tvOS, Raspberry Pi, etc.)
and connects directly to the host PC.

**Pros:**
- Native hardware-decoded video (very low latency, high quality).
- Supports gamepad, keyboard, and mouse input natively.
- No relay server needed; peer-to-peer on the local network, or via NAT
  traversal when the host has a public IP or port-forwarding.

**Cons:**
- Viewer must install Moonlight.
- PC must be reachable (public IP, port forwarding, or VPN).
- First-time pairing requires the viewer to enter a PIN shown on screen;
  `sunshine-agent` can automate this with `-pair-pin` if the PIN is known
  in advance (e.g. fixed configuration), but a dynamic PIN relay is a TODO.

### Option B: Web relay (current limitation)

PlayGate does not currently provide a browser-based relay for PC mode.
The WebRTC pipeline (`internal/rtc`) handles console streaming (HDMI capture)
but is separate from Sunshine's RTSP stream.

Bridging Sunshine's RTSP output to a WebRTC browser client is **not yet
implemented** — see Known Limitations below.

## Known Limitations

1. **PIN relay is manual / static.**  In the current implementation the
   pairing PIN must be configured statically via `-pair-pin`.  A full
   integration would have `playgate-server` relay the PIN from the viewer's
   Moonlight app to the agent.  Marked `TODO` in `cmd/sunshine-agent/main.go`.

2. **No browser streaming.**  PC-mode viewers must use the Moonlight app.
   A WebRTC bridge from Sunshine's RTSP output to a browser tab is a future
   work item.

3. **Single active viewer only.**  `cmd/sunshine-agent` uses
   `session.PolicyReject`: a second viewer is rejected while the first is
   active.  Sunshine itself supports only one streaming client at a time, so
   queueing would not help unless the agent forcefully kicks the first viewer
   first.

4. **Idle detection is approximated.**  The agent kicks via `POST /api/apps/close`
   on `EventIdleKicked`, but PlayGate does not currently receive Moonlight
   input events — `IdleTimeout` in `session.Config` would require an
   input event source.  For PC mode, disable idle timeout or wire up a
   separate Moonlight-input listener.

5. **Sunshine version compatibility.**  The endpoint table is based on v0.23.x.
   Verify paths against your installed version; all paths are overridable via
   `ClientConfig.Override*` without code changes.

## Integration Suggestion: Merging into `cmd/host`

When the time comes to merge PC mode into the main host binary
(`cmd/host`), the recommended approach:

1. **Add a `mode` field to `internal/config`** (e.g. `mode: pc | console`).
   The PC mode section should include `sunshine_url`, `sunshine_user`,
   `sunshine_pass`, and optionally `pair_pin`.

2. **In `internal/host.New()`**, branch on `cfg.Mode`:
   - `console` — wire the existing encoder/rtc/input pipeline (unchanged).
   - `pc` — skip encoder/rtc/input; instead create a
     `sunshine.HTTPClient` and a `sunshine.Agent`, then add the agent
     to the errgroup alongside the session manager.

3. **Remove `cmd/sunshine-agent`** once the host binary supports both modes
   (or keep it as a lightweight standalone for operators who don't want the
   full binary).

4. **JWT token delivery** in PC mode: the current standalone binary reads
   tokens from `-token` flag or stdin.  In the integrated host the signaling
   module (T6) already handles token delivery — wire its `Claim()` call to
   the shared `session.Manager`.

### Minimal diff sketch

```go
// internal/host/host.go (illustrative only — do not apply directly)
import "github.com/playgate/playgate-host/internal/sunshine"

func New(log *slog.Logger, cfg *config.Config) (*Host, error) {
    // ... existing setup ...
    if cfg.Mode == "pc" {
        ctrl := sunshine.NewHTTPClient(sunshine.ClientConfig{
            BaseURL:            cfg.Sunshine.URL,
            Username:           cfg.Sunshine.User,
            Password:           cfg.Sunshine.Pass,
            InsecureSkipVerify: cfg.Sunshine.Insecure,
        })
        ag, err := sunshine.NewAgent(sunshine.AgentConfig{
            Controller: ctrl,
            Events:     sessionMgr.Events(),
            PairPIN:    cfg.Sunshine.PairPIN,
            Log:        log,
        })
        if err != nil { return nil, err }
        g.Go(func() error { return ag.Run(ctx) })
        return h, nil   // skip encoder/rtc/input
    }
    // ... console mode pipeline ...
}
```
