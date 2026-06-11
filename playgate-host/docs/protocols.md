# PlayGate Host — Protocol Reference

Canonical specification for all wire formats used between playgate-host,
the browser frontend (T11 / playgate-web), and the NXBT Python daemon.

All byte layouts listed here are derived directly from the source code
(as of the commit at which this file was created). When in doubt, the Go
source in `internal/` is authoritative; this document is a convenience
reference for frontend and daemon implementers.

---

## 1. InputCommand — binary wire format (WebRTC "input" DataChannel)

Source: `internal/rtc/input_codec.go`

### Summary

Controller state snapshots are sent from the browser to the host over the
unreliable, unordered WebRTC DataChannel with label **`"input"`**. Each
message is exactly **13 bytes**, little-endian throughout.

The browser must send one frame per 60 Hz interval (~16.67 ms) whenever the
user holds any input. Frames sent while the viewer does not hold control are
discarded by the session gate on the host.

### Byte layout

```
offset  size  type    field     description
------  ----  ------  --------  -----------------------------------------------
0       1     uint8   version   Wire format version. Must equal 0x01 (InputWireVersion).
                                Frames with any other version byte are dropped.
1       4     uint32  buttons   Button bitmask (little-endian). See Section 2.
5       2     int16   lx        Left  stick X axis (little-endian). See axis scaling.
7       2     int16   ly        Left  stick Y axis (little-endian).
9       2     int16   rx        Right stick X axis (little-endian).
11      2     int16   ry        Right stick Y axis (little-endian).
------  ----
total   13 bytes
```

All multi-byte fields are **little-endian** (least-significant byte first).

### Axis scaling

Each normalised axis value in `[-1.0, 1.0]` is mapped to a signed 16-bit
integer by:

```
int16_value = clamp(round(float_value × 32767), −32767, +32767)
```

Decoding reverses this:

```
float_value = int16_value / 32767.0
```

**Note:** The value `−32768` (`0x8000`) is never produced by a well-behaved
encoder. A host receiver clamps it to `−1.0` on decode rather than producing
`−1.0000305...`.

Constants:

| Name              | Value  |
|-------------------|--------|
| `InputWireVersion` | `1`   |
| `InputWireSize`    | `13`  |
| `AxisScale`        | `32767` |

### Encoding example (JavaScript)

```js
const INPUT_WIRE_VERSION = 1;
const AXIS_SCALE = 32767;

function encodeInputCommand(buttons, lx, ly, rx, ry) {
  const buf = new ArrayBuffer(13);
  const v   = new DataView(buf);
  v.setUint8 (0,  INPUT_WIRE_VERSION);
  v.setUint32(1,  buttons >>> 0, /* littleEndian= */ true);
  v.setInt16 (5,  Math.round(Math.max(-AXIS_SCALE, Math.min(AXIS_SCALE, lx * AXIS_SCALE))), true);
  v.setInt16 (7,  Math.round(Math.max(-AXIS_SCALE, Math.min(AXIS_SCALE, ly * AXIS_SCALE))), true);
  v.setInt16 (9,  Math.round(Math.max(-AXIS_SCALE, Math.min(AXIS_SCALE, rx * AXIS_SCALE))), true);
  v.setInt16 (11, Math.round(Math.max(-AXIS_SCALE, Math.min(AXIS_SCALE, ry * AXIS_SCALE))), true);
  return buf;
}
```

### No timestamp in wire format

The wire format carries **no timestamp**. The host stamps each received frame
with its own `time.Now()` at decode time. Because the DataChannel is
unreliable and unordered, a sender-side timestamp would add 8 bytes with
minimal benefit — the host treats each frame as the latest-known desired state
at receive time.

---

## 2. Button bitmask

Source: `internal/core/types.go` (Go constants) and `nxbt-daemon/nxbtd.py` (`BUTTON_MAP`).

The `buttons` field in the InputCommand is a `uint32` bitmask. Each bit
corresponds to one button; the bit layout is identical in both the Go side
and the NXBT daemon.

### Bit assignment table

| Bit | Mask (`uint32`) | Go constant      | NXBT name    | Description           |
|----:|-----------------|------------------|--------------|-----------------------|
|   0 | `0x00000001`    | `ButtonA`        | `"A"`        | A button              |
|   1 | `0x00000002`    | `ButtonB`        | `"B"`        | B button              |
|   2 | `0x00000004`    | `ButtonX`        | `"X"`        | X button              |
|   3 | `0x00000008`    | `ButtonY`        | `"Y"`        | Y button              |
|   4 | `0x00000010`    | `ButtonL`        | `"L"`        | Left shoulder         |
|   5 | `0x00000020`    | `ButtonR`        | `"R"`        | Right shoulder        |
|   6 | `0x00000040`    | `ButtonZL`       | `"ZL"`       | Left  trigger         |
|   7 | `0x00000080`    | `ButtonZR`       | `"ZR"`       | Right trigger         |
|   8 | `0x00000100`    | `ButtonPlus`     | `"PLUS"`     | + button              |
|   9 | `0x00000200`    | `ButtonMinus`    | `"MINUS"`    | − button              |
|  10 | `0x00000400`    | `ButtonHome`     | `"HOME"`     | Home button           |
|  11 | `0x00000800`    | `ButtonCapture`  | `"CAPTURE"`  | Capture/screenshot    |
|  12 | `0x00001000`    | `ButtonLStick`   | `"L_STICK"`  | Left  stick click     |
|  13 | `0x00002000`    | `ButtonRStick`   | `"R_STICK"`  | Right stick click     |
|  14 | `0x00004000`    | `ButtonDpadUp`   | `"DPAD_UP"`  | D-pad up              |
|  15 | `0x00008000`    | `ButtonDpadDown` | `"DPAD_DOWN"`| D-pad down            |
|  16 | `0x00010000`    | `ButtonDpadLeft` | `"DPAD_LEFT"`| D-pad left            |
|  17 | `0x00020000`    | `ButtonDpadRight`| `"DPAD_RIGHT"`| D-pad right          |

Bits 18–31 are reserved and must be zero.

**Consistency:** The Go `core.Button*` constants (defined with `iota` starting
at bit 0) and the Python `BUTTON_MAP` in `nxbt-daemon/nxbtd.py` use the same
mask values and NXBT name strings. The automated test
`TestButtonsToNxbt.test_bitmask_consistency_with_core_types` in
`nxbt-daemon/test_protocol.py` verifies this at every CI run.

---

## 3. Go ↔ NXBT daemon — Unix socket JSON protocol

Source: `internal/input/nxbt/protocol.go`, `nxbt-daemon/nxbtd.py`

The Go host process connects to the NXBT Python daemon over a Unix domain
socket at `/run/nxbt/nxbt.sock` (configurable). Messages are
**newline-delimited JSON** (one JSON object per line, UTF-8, terminated with
`\n`, no trailing whitespace). The socket is a SOCK_STREAM; messages are
framed by scanning for `\n`.

### 3.1 Go → daemon

#### `input` — controller state snapshot

Sent by Go to forward the current controller state. Sent at up to 120 Hz
(configurable via `WithRateHz`); the coalescer drops intermediate frames when
the daemon cannot keep up.

```json
{"type":"input","buttons":1,"lx":0.5,"ly":-0.5,"rx":0.0,"ry":0.0}
```

| Field     | Type    | Description                                                   |
|-----------|---------|---------------------------------------------------------------|
| `type`    | string  | Always `"input"`.                                             |
| `buttons` | uint32  | Button bitmask (see Section 2).                              |
| `lx`      | float64 | Left  stick X, normalised `[-1.0, 1.0]`.                    |
| `ly`      | float64 | Left  stick Y, normalised `[-1.0, 1.0]`.                    |
| `rx`      | float64 | Right stick X, normalised `[-1.0, 1.0]`.                    |
| `ry`      | float64 | Right stick Y, normalised `[-1.0, 1.0]`.                    |

All axis fields default to `0` if absent. A daemon MUST NOT error on missing
axis fields.

#### `ping` — keepalive

Sent every 5 seconds to verify the socket is alive.

```json
{"type":"ping"}
```

### 3.2 Daemon → Go

#### `status` — Bluetooth link state change

Sent whenever the Bluetooth connection state changes.

```json
{"type":"status","state":"connected","detail":"Switch paired to 01:23:45:67:89:AB"}
```

| Field    | Type   | Values                                        |
|----------|--------|-----------------------------------------------|
| `type`   | string | Always `"status"`.                            |
| `state`  | string | `"connecting"` \| `"connected"` \| `"disconnected"` |
| `detail` | string | Human-readable detail string. Optional / may be `""`. |

#### `pong` — keepalive reply

Sent immediately in response to a `ping`.

```json
{"type":"pong"}
```

### 3.3 Notes

- Only one Go client is expected at a time; a new connection closes the previous session.
- The daemon reconnects to the Switch automatically with exponential back-off.
- In mock mode (`--mock` flag or if nuxbt cannot be imported) the daemon honours
  the full protocol but logs inputs rather than forwarding them to a real Switch.

---

## 4. SessionEvent — control DataChannel JSON (WebRTC "control" channel)

Source: `internal/session/event.go`

Session lifecycle events are pushed from the host to each viewer over the
reliable, ordered WebRTC DataChannel with label **`"control"`**.

Each event is a UTF-8 JSON object sent as a text message.

### Event shape

```json
{
  "kind":              "granted",
  "viewer_id":         "cafe1234",
  "remaining_seconds": 120,
  "queue_position":    0,
  "ts":                1718000000
}
```

| Field              | Type   | Description                                                        |
|--------------------|--------|--------------------------------------------------------------------|
| `kind`             | string | Event type (see table below).                                      |
| `viewer_id`        | string | The viewer this event concerns (hex string from JWT `viewer_id`).  |
| `remaining_seconds`| int    | Seconds of control remaining. `0` for non-active events.           |
| `queue_position`   | int    | 1-based queue position. `0` when not queued.                      |
| `ts`               | int64  | Unix timestamp (seconds) of the host's wall clock when emitted.    |

### Event kinds

| `kind`        | When emitted                                              | `remaining_seconds` | `queue_position` |
|---------------|-----------------------------------------------------------|---------------------|------------------|
| `granted`     | Viewer gains control (initially or after queue promotion) | Full session length | `0`              |
| `expired`     | Session timer reaches zero normally                       | `0`                 | `0`              |
| `idle_kicked` | Viewer removed for inactivity                             | `0`                 | `0`              |
| `queued`      | Viewer accepted but must wait for current session to end  | `0`                 | ≥ 1              |
| `tick`        | Periodic countdown (every `TickInterval`, default 1 s)   | Counting down       | `0`              |

### Frontend recommended behaviour

- On `granted`: start showing a countdown timer using `remaining_seconds`.
  If `viewer_id` matches the local viewer, activate controller input.
- On `tick`: update the countdown display.
- On `expired` / `idle_kicked`: disable controller input; show a message.
- On `queued`: show "You are #N in queue" using `queue_position`.

**Important:** the T6 module SHOULD send each event only to the viewer
identified by `viewer_id`. Events for other viewers may be safely forwarded
for observability but must not enable controller input for the wrong viewer.

---

## 5. Session JWT claims

Source: `internal/session/jwt.go`, `internal/session/jwt_test.go`

playgate-server issues short-lived JWTs granting a specific viewer permission
to control a specific room. The host validates the JWT before accepting a
`Claim` call.

### Algorithm

**EdDSA (Ed25519)** — RFC 8037. Header:

```json
{"alg":"EdDSA","typ":"JWT"}
```

Any other `alg` or `typ` value is rejected.

### Payload (claims)

```json
{
  "iss": "playgate-server",
  "iat": 1718000000,
  "exp": 1718000120,
  "room_id": "deadbeef",
  "viewer_id": "cafe1234",
  "session_seconds": 120
}
```

| Claim             | Type   | Required | Description                                                    |
|-------------------|--------|----------|----------------------------------------------------------------|
| `iss`             | string | No†      | Issuer. Conventionally `"playgate-server"`. Not verified by host. |
| `iat`             | int64  | No†      | Issued-at Unix timestamp. Not verified by host.                |
| `exp`             | int64  | Yes      | Expiry Unix timestamp. Token rejected if `now >= exp`.         |
| `room_id`         | string | Yes      | Must match the host's configured `RoomID`.                     |
| `viewer_id`       | string | Yes      | Opaque viewer identifier (used as the key for gates/events).   |
| `session_seconds` | int    | Yes      | Duration of the control session in seconds. Must be > 0.       |

† `iss` and `iat` are not validated by the host but should be included by
  playgate-server for auditability.

### Serialisation

Compact JWS format: `BASE64URL(header) + "." + BASE64URL(payload) + "." + BASE64URL(signature)`.
All base64url parts have no padding (`=`). The signed message is the ASCII
string `"<header_b64>.<payload_b64>"`.

### Key format

The public key is a 32-byte Ed25519 public key encoded as base64 (standard or
URL-safe alphabet, with or without padding). It is configured on the host via
`Config.PublicKeyBase64` or `Config.PublicKeyFile`.

---

## 6. SDP signaling — base64 encoding

Source: `internal/rtc/signaling.go`

For manual copy-paste signaling (cmd/rtctest) and the Cloudflare Worker (T7),
SDP offer/answer objects are serialised as:

```
BASE64_STD( JSON( {"type": "<offer|answer>", "sdp": "<SDP string>"} ) )
```

Standard (non-URL) base64 with padding (`=`) is used (`base64.StdEncoding`).

The browser side (`cmd/rtctest/index.html`) must use the same encoding:

```js
function encodeSDP(desc) {
  return btoa(JSON.stringify({ type: desc.type, sdp: desc.sdp }));
}
function decodeSDP(b64) {
  return new RTCSessionDescription(JSON.parse(atob(b64.trim())));
}
```
