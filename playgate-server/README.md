# playgate-server

HTTP API server for the PlayGate platform. Handles host registration, room management, token issuance, session JWT signing, and queue tracking.

Signaling and TURN are handled externally (Cloudflare Workers + Cloudflare Realtime). This server is pure business logic.

## Quick start

```sh
# Run (defaults: addr=:8080, db=playgate.db, key=ed25519.pem)
go run . 

# With explicit config
go run . -addr=:8080 -db=/data/playgate.db -key=/secrets/ed25519.pem

# Environment variables
PLAYGATE_ADDR=:8080 PLAYGATE_DB=./playgate.db PLAYGATE_KEY=./ed25519.pem go run .
```

On first run the server generates an ed25519 private key and saves it to `-key` path. The corresponding public key is printed to stdout and served at `GET /api/public-key`.

## Configuration

| Flag | Env var | Default | Description |
|------|---------|---------|-------------|
| `-addr` | `PLAYGATE_ADDR` | `:8080` | TCP listen address |
| `-db` | `PLAYGATE_DB` | `playgate.db` | SQLite database path |
| `-key` | `PLAYGATE_KEY` | `ed25519.pem` | ed25519 private key PEM path |

## JWT Claims Format

Session tokens are compact JWTs signed with **EdDSA (ed25519)**. The `playgate-host` should verify signatures with the public key from `GET /api/public-key`.

**Header**
```json
{"alg":"EdDSA","typ":"JWT"}
```

**Payload**
```json
{
  "iss": "playgate-server",
  "iat": 1700000000,
  "exp": 1700000090,
  "room_id": "790a7ace91eb4e4315ddb3e04e614739",
  "viewer_id": "9e601e2806bf2663d0b54e85ec9f6dff",
  "session_seconds": 90
}
```

| Claim | Type | Description |
|-------|------|-------------|
| `iss` | string | Always `"playgate-server"` |
| `iat` | unix timestamp | Issued at |
| `exp` | unix timestamp | Expiry (`iat + session_seconds`) |
| `room_id` | string | Hex room identifier |
| `viewer_id` | string | Anonymous per-session viewer identifier (hex) |
| `session_seconds` | int | Authorised control duration in seconds |

The `playgate-host` verifies: signature, `exp` not past, `room_id` matches local config. It does **not** call back to this server for verification.

## API Endpoints

All request/response bodies are JSON. Host-authenticated endpoints require `Authorization: Bearer <api_key>`.

---

### `GET /api/public-key`

Returns the server's ed25519 public key. The `playgate-host` fetches this once on startup.

**Response 200**
```json
{
  "algorithm": "EdDSA",
  "public_key": "ofu1efGsvOQ0LQTJOqvpiwAqWLJADupdOZP+zGpvAmE="
}
```
`public_key` is standard base64-encoded (32 bytes).

---

### `POST /api/hosts/register`

Register a new host (streamer). Returns a permanent API key. **No authentication required.**

**Request**
```json
{"name": "MyChannel"}
```

**Response 201**
```json
{
  "host_id": "7f644e3efabf05018fea57b48481ea42",
  "api_key": "39d5429b181d38858cfde89d6b8d43431d03b128ff54f7fdba44f4d96b9f8b85"
}
```

---

### `POST /api/rooms`

Create a room. **Requires host API key.**

**Request**
```json
{
  "name": "Zelda Stream",
  "session_seconds": 90
}
```
`session_seconds` defaults to 60 if omitted.

**Response 201**
```json
{
  "id": "790a7ace91eb4e4315ddb3e04e614739",
  "name": "Zelda Stream",
  "session_seconds": 90,
  "online": false,
  "current_viewer": null,
  "queue_depth": 0
}
```

---

### `GET /api/rooms/{id}`

Query room status. **Public.**

**Response 200**
```json
{
  "id": "790a7ace91eb4e4315ddb3e04e614739",
  "name": "Zelda Stream",
  "session_seconds": 90,
  "online": true,
  "current_viewer": "9e601e2806bf2663d0b54e85ec9f6dff",
  "queue_depth": 3
}
```

`queue_depth` is the count of active (non-expired) sessions in this room.

---

### `POST /api/rooms/{id}/heartbeat`

Host reports its online status and current controlling viewer. **Requires host API key.**

**Request**
```json
{
  "online": true,
  "current_viewer": "9e601e2806bf2663d0b54e85ec9f6dff"
}
```
Set `current_viewer` to `null` when nobody is controlling.

**Response 200**
```json
{"status": "ok"}
```

---

### `POST /api/rooms/{id}/tokens`

Streamer issues a batch of redeemable token codes to distribute to viewers. **Requires host API key.**

Token codes are opaque hex strings. The streamer distributes them to viewers (e.g. via chat command, overlay, or OBS widget). Viewers redeem codes to obtain session JWTs.

**Request**
```json
{"count": 5}
```
`count` must be between 1 and 100.

**Response 201**
```json
{
  "codes": [
    "5b5f6121949637010021",
    "8258d5d837809625b053",
    "be1d3ff29bc3985d6475",
    "a1b2c3d4e5f678901234",
    "fedcba9876543210abcd"
  ]
}
```

---

### `DELETE /api/tokens/{code}`

Revoke an unused token. Only the issuing host can revoke. **Requires host API key.**

Already-redeemed tokens cannot be revoked (returns 409).

**Response 204** (no body)

---

### `POST /api/tokens/{code}/redeem`

Viewer redeems a token code to obtain a session JWT. **No authentication required** — the token code itself is the proof.

A token can only be redeemed once.

**Request** (empty body OK)
```json
{}
```

**Response 200**
```json
{
  "session_token": "eyJhbGciOiJFZERTQSIsInR5cCI6IkpXVCJ9.eyJpc3MiOiJwbGF5Z2F0ZS1zZXJ2ZXIiLCJpYXQiOjE3ODEwNzI0OTEsImV4cCI6MTc4MTA3MjU4MSwicm9vbV9pZCI6Ijc5MGE3YWNlOTFlYjRlNDMxNWRkYjNlMDRlNjE0NzM5Iiwidmlld2VyX2lkIjoiOWU2MDFlMjgwNmJmMjY2M2QwYjU0ZTg1ZWM5ZjZkZmYiLCJzZXNzaW9uX3NlY29uZHMiOjkwfQ.4LryxDPlUlwb7Dvca-xPgIMlZBlhKLcEf1lDu2I99t7Djy0RUBJF6wyI9jmaCB-HyOfWKLYwCsGGbvTGflHSCw",
  "queue_position": 1,
  "expires_at": 1781072581,
  "room_id": "790a7ace91eb4e4315ddb3e04e614739",
  "viewer_id": "9e601e2806bf2663d0b54e85ec9f6dff"
}
```

| Field | Description |
|-------|-------------|
| `session_token` | Signed JWT to pass to `playgate-host` via WebRTC data channel |
| `queue_position` | 1-based position in the room's redemption queue |
| `expires_at` | Unix timestamp when the session expires |
| `room_id` | Which room this session is for |
| `viewer_id` | Opaque anonymous viewer identifier embedded in JWT |

**Error codes**

| Status | Meaning |
|--------|---------|
| 404 | Code not found |
| 409 | Already redeemed |
| 410 | Revoked |

---

## Integration Guide

### playgate-host integration

1. On startup, fetch `GET /api/public-key` and load the base64 public key.
2. Accept the `session_token` JWT from the viewer via WebRTC data channel.
3. Verify: parse JWT, check `alg=EdDSA`, verify ed25519 signature, check `exp > now`, check `room_id` matches local config.
4. Grant control for `session_seconds` seconds.
5. Send heartbeat `POST /api/rooms/{id}/heartbeat` every 30 s with `current_viewer` set to the active viewer's `viewer_id`.

### Web/overlay integration

1. Streamer calls `POST /api/rooms/{id}/tokens` to obtain codes.
2. Streamer distributes codes to viewers (chat, Twitch channel points redemption, etc.).
3. Viewer calls `POST /api/tokens/{code}/redeem` from the browser.
4. Browser receives `session_token` JWT and `queue_position`.
5. Browser connects to Cloudflare Workers signaling with the JWT.

### Legal note

Token codes are issued **by the streamer** and distributed to viewers. The platform never sells tokens to viewers directly. PlayGate is a SaaS tool (monthly subscription from streamers), not a game operator.

## Database Schema

| Table | Description |
|-------|-------------|
| `hosts` | Registered streamers with their API keys |
| `rooms` | Rooms owned by hosts, with online/heartbeat state |
| `tokens` | Redeemable codes issued per room |
| `sessions` | Redeemed sessions with JWT, queue position, expiry |

## Development

```sh
go test ./...          # run tests
go vet ./...           # static analysis
go build ./...         # compile check
```
