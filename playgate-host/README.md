# playgate-host

The PlayGate **Host** runs on the streamer's machine (Linux / Raspberry Pi). It
captures the Nintendo Switch's HDMI output, encodes it to H.264, streams it to
viewers over a single WebRTC connection, and feeds controller commands received
on that same connection back into the Switch via a virtual controller.

The full pipeline (T1–T6, T9) is wired: v4l2 capture → ffmpeg H.264 encoding →
Pion WebRTC, with controller input flowing back through the session gate to the
NXBT bridge, and signaling via the Cloudflare Worker (`playgate-signaling`).
Wire-format contracts live in [`docs/protocols.md`](docs/protocols.md).

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
| `internal/host/` | Wires modules into the pipeline (video router, input sink, viewer connection loop) and supervises them via `errgroup`. |
| `internal/capture/v4l2/` | Pure-Go V4L2 capture (raw ioctl on `x/sys/unix`, Linux only). |
| `internal/capture/synthetic/` | Pure-Go test-pattern source for development on any OS. |
| `internal/encoder/ffmpeg/` | H.264 encoding via an ffmpeg subprocess (zerolatency, drop-oldest backpressure). |
| `internal/rtc/` | Pion peer: H.264 sample track, `input` (unreliable) + `control` (reliable) DataChannels. |
| `internal/signaling/` | stdlib HTTP client for the signaling Worker (offer/answer/ICE + TURN credentials). |
| `internal/session/` | ed25519-JWT session gate: single controller, expiry, idle kick, FIFO queue. |
| `internal/input/nxbt/` | Unix-socket bridge to the NXBT Python daemon (`nxbt-daemon/`), rate-limited. |
| `internal/metrics/` | Pipeline latency collector (capture→encode→track, p50/p95). |

## Running

```sh
cp config.example.yaml config.yaml   # edit for your machine
go run ./cmd/host -config config.yaml
# add -debug for debug-level logs
```

Press `Ctrl+C` to shut down; all module goroutines exit cleanly.

**Dev mode (no capture card / Switch / Linux):** set `capture.source: synthetic`
and `input.target: log` — the host then runs on any OS, streams a generated test
pattern, and logs received controller commands instead of driving a Switch.

**End-to-end test:** deploy or `wrangler dev` the signaling Worker, start the
host, then open `web-test/index.html`, enter the Worker URL + room id (+ session
JWT when the gate is enabled) and press Connect. Keyboard input is packed into
13-byte InputCommand frames at 60 Hz (see `docs/protocols.md`).

## Latency

`internal/metrics` logs per-stage pipeline latency (capture→encode, encode→track
write) p50/p95 every `metrics.report_interval_seconds`. The control channel
echoes `{"kind":"ping","ts":N}` as `pong`, so the test page shows live
application-level RTT.

Full glass-to-glass latency must be measured on real hardware: film the Switch
screen and the browser window side by side with a millisecond clock overlay and
compare frames. Record baselines here:

| Stage | Target | Measured (RPi 4) | Measured (x86) |
|-------|--------|------------------|----------------|
| capture → encoder in | — | _TBD_ | _TBD_ |
| encoder in → track write | — | _TBD_ | _TBD_ |
| control-channel RTT | — | _TBD_ | _TBD_ |
| glass-to-glass | ≤ 150 ms | _TBD_ | _TBD_ |

## Development

```sh
go build ./...
go vet ./...
go test ./...
GOOS=linux go build ./...   # cross-check the Linux-only capture/input paths
```

`internal/host` includes integration tests that run the synthetic pipeline end
to end (capture → fake encoder → rtc loopback, input → gate → fake target) and
assert all goroutines exit within a deadline (no leaks).
