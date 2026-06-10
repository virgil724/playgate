# playgate-host

The PlayGate **Host** runs on the streamer's machine (Linux / Raspberry Pi). It
captures the Nintendo Switch's HDMI output, encodes it to H.264, streams it to
viewers over a single WebRTC connection, and feeds controller commands received
on that same connection back into the Switch via a virtual controller.

This repository currently contains the **T1 skeleton**: the module/lifecycle
architecture, core types and interfaces, config loading, and stub modules. The
real capture/encode/transport/input logic lands in later tasks.

## Architecture

The host is a set of independent modules. Each module runs its own goroutine
loop, communicates with the next module through a channel, and is shut down via
a shared `context.Context`. `main` only loads config and wires modules together;
`internal/host` supervises them with an `errgroup`.

```
                 raw frames          H.264 packets         controller cmds
 CaptureSource ───VideoFrame──▶ Encoder ──EncodedPacket──▶ WebRTC ──InputCommand──▶ InputTarget
 (T2: v4l2)                     (T3: ffmpeg)               (T4: Pion)               (T5: NXBT)
                                                              │
                                                              └─ media track + DataChannel ⇄ viewer

 Session (T9) — control-plane module (who currently holds control); no pipeline channels.
```

All five modules run concurrently inside one `errgroup`. If any module returns a
fatal error, the group context is cancelled and every other module shuts down.
A `SIGINT`/`SIGTERM` (Ctrl+C) cancels the root context for a graceful shutdown.

### Channel ownership rule

The **producer owns the channel**: the goroutine that creates a channel is its
only writer and closes it exactly once when its `Run` loop returns. Consumers
only read and must tolerate a closed channel. Lifetime is bounded by the
`context.Context` passed to `Run`. This rule is documented on each type in
`internal/core`.

## Layout

| Path | Purpose |
|------|---------|
| `cmd/host/` | Entrypoint: load config, install signal handler, wire + run modules. |
| `internal/core/` | Shared types (`VideoFrame`, `EncodedPacket`, `InputCommand`) and interfaces (`Module`, `CaptureSource`, `InputTarget`). No internal deps. |
| `internal/config/` | YAML config loading (`gopkg.in/yaml.v3`) with defaults + validation. |
| `internal/host/` | Wires modules into the pipeline and supervises them via `errgroup`. |
| `internal/module/` | Stub modules: `capture`, `encoder`, `webrtc`, `input`, `session`. |

## Running

```sh
cp config.example.yaml config.yaml   # edit for your machine
go run ./cmd/host -config config.yaml
# add -debug for debug-level logs
```

Press `Ctrl+C` to shut down; all module goroutines exit cleanly.

## Development

```sh
go build ./...
go vet ./...
go test ./...
```

`internal/host` includes a test that starts every stub module, cancels the
context, and asserts all goroutines exit within a deadline (no leaks).
