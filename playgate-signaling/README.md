# playgate-signaling

Cloudflare Workers-based WebRTC signaling server for PlayGate.

Implements **T7** (SDP offer/answer + ICE candidate exchange via a per-room Durable Object, with HTTP polling, HTTP long-poll, and WebSocket push) and **T8** (short-lived Cloudflare Realtime TURN credential issuance).

Room state lives in a per-room **Durable Object** (`RoomDO`), selected by `env.ROOMS.idFromName(roomId)`. Workers KV is no longer used — the DO holds each room's per-peer message queues in SQLite-backed storage and pushes new messages over WebSocket / resolves long-polls instantly, so an idle host and an open browser tab no longer burn polling reads.

---

## API Reference

### Health Check

```
GET /healthz
```

No auth required. Returns `{ "status": "ok" }` with HTTP 200.

---

### T7 — Signaling Endpoints

#### Push a message

```
POST /rooms/{roomId}/{peer}
Authorization: Bearer <session-token>   (optional while AUTH_DISABLED=true)
Content-Type: application/json

<any JSON — SDP offer/answer or ICE candidate>
```

- `peer` must be `host` or `viewer`.
- Appends the payload to the sender's queue inside the room's Durable Object.
- A host SDP **offer** starts a fresh session: it replaces the host queue (seq continuity preserved as `lastSeq + 1`) and deletes the viewer queue, so stale ICE/answers from a dead peer never survive a new offer.

**Response 201:**
```json
{ "seq": 0, "ts": "2024-01-01T00:00:00.000Z" }
```

**Example — host posts SDP offer:**
```bash
curl -X POST https://<worker>.workers.dev/rooms/my-room/host \
  -H "Content-Type: application/json" \
  -d '{"type":"offer","sdp":"v=0\r\no=- ...\r\n"}'
```

---

#### Poll for messages

```
GET /rooms/{roomId}/{peer}?since=<lastSeq>[&wait=<seconds>]
Authorization: Bearer <session-token>
```

Returns messages posted by the **other** peer.  
`since` (optional): last `seq` the caller has already processed; messages with `seq > since` are returned.  
Omit `since` (or pass `-1`) to receive all messages.  
`wait` (optional, NEW): seconds to hold the request inside the room DO when there are **no** new messages, up to a server cap of **25 s**. When a new message arrives within that window the request returns immediately; otherwise it returns an empty `messages` list on timeout. `wait` absent or `0` → immediate response (classic polling).

**Response 200:**
```json
{
  "messages": [
    {
      "seq": 0,
      "ts": "2024-01-01T00:00:00.000Z",
      "payload": { "type": "offer", "sdp": "v=0\r\n..." }
    }
  ],
  "nextSince": 0
}
```

**Example — viewer long-polls for host's offer (push-like, no busy loop):**
```bash
curl "https://<worker>.workers.dev/rooms/my-room/viewer?since=-1&wait=25"
```

---

#### Subscribe over WebSocket (push)

```
GET /rooms/{roomId}/{peer}/ws
Upgrade: websocket
Authorization: Bearer <session-token>
```

Opens a WebSocket into the room DO using the **hibernation API**. On connect the
server immediately sends the **other** peer's current backlog, one frame per
message. Thereafter every new message from the other peer is pushed live.

- **Server→client frame** (one per message):
  ```json
  { "seq": 0, "ts": "2024-01-01T00:00:00.000Z", "payload": { "type": "offer", "sdp": "..." } }
  ```
- **Client→server frame**: a raw JSON payload — the same body you would `POST`.
  The server assigns `seq`/`ts`, applies the host offer-reset rule, stores it,
  and pushes it as a `{seq,ts,payload}` frame to all of the other peer's sockets
  (and resolves any pending long-poll for that peer).
- Malformed JSON frames are ignored (the server may reply `{"error":"bad json"}`)
  and never close the socket.
- Multiple sockets per peer are allowed; pushes fan out to all of them.

**Browser usage:**
```js
const ws = new WebSocket(`wss://<worker>.workers.dev/rooms/my-room/viewer/ws`);
ws.onmessage = (e) => {
  const { seq, ts, payload } = JSON.parse(e.data);
  // handle SDP offer / ICE candidate from the host
};
// send an answer / ICE candidate:
ws.send(JSON.stringify({ type: "answer", sdp: "v=0\r\n..." }));
```

---

### T8 — TURN Credentials

```
POST /turn/credentials
Authorization: Bearer <session-token>
Content-Type: application/json

{ "ttl": 3600 }   (optional, default 86400, max 86400)
```

Calls the Cloudflare Realtime TURN credential generation API and returns ICE server configuration ready for `RTCPeerConnection`.

**Response 200:**
```json
{
  "iceServers": [
    { "urls": "stun:stun.cloudflare.com:3478" },
    {
      "urls": [
        "turn:turn.cloudflare.com:3478?transport=udp",
        "turn:turn.cloudflare.com:3478?transport=tcp",
        "turns:turn.cloudflare.com:5349?transport=tcp"
      ],
      "username": "1234567890:abc...",
      "credential": "xyz..."
    }
  ],
  "ttl": 86400
}
```

**Browser usage:**
```js
const { iceServers } = await fetch("/turn/credentials", {
  method: "POST",
  headers: { Authorization: `Bearer ${sessionToken}` },
}).then((r) => r.json());

const pc = new RTCPeerConnection({ iceServers });
```

---

## Authentication

The auth layer is a configurable stub, ready to be replaced with ed25519 JWT verification once `playgate-server` issues signed tokens.

| State | Behaviour |
|---|---|
| `AUTH_DISABLED=true` | All requests pass (dev/testing) |
| `SESSION_SECRET` not set | All requests pass (permissive default) |
| `SESSION_SECRET` set | Requests must carry `Authorization: Bearer <token>` where the token is `HMAC-SHA256("playgate-session", SESSION_SECRET)` as a hex string |

`GET /healthz` always bypasses auth.

---

## Deployment

### 1. Prerequisites

- Node 18+ and `npm`
- A [Cloudflare account](https://dash.cloudflare.com/) (free tier is fine)
- `wrangler` CLI (`npm i -g wrangler` or use `npx wrangler`)

### 2. Durable Object (no KV namespace needed)

There is **no KV namespace to create** anymore. Room state lives in the
per-room Durable Object `RoomDO`, configured in `wrangler.toml`:

```toml
[[durable_objects.bindings]]
name = "ROOMS"
class_name = "RoomDO"

# SQLite-backed DO classes are REQUIRED on the free plan.
[[migrations]]
tag = "v1"
new_sqlite_classes = ["RoomDO"]
```

The `[[migrations]]` block with `new_sqlite_classes` is applied automatically on
the first `wrangler deploy`; no manual namespace provisioning is required.

### 3. Set secrets

```bash
# Cloudflare Realtime TURN (T8)
npx wrangler secret put TURN_KEY_ID
npx wrangler secret put TURN_KEY_API_TOKEN

# Optional — enables shared-secret stub auth
npx wrangler secret put SESSION_SECRET
```

To obtain `TURN_KEY_ID` and `TURN_KEY_API_TOKEN`:
1. Go to Cloudflare Dashboard → Realtime → TURN
2. Create a new TURN key
3. Copy the Key ID and the API token

### 4. Deploy

```bash
npm install
npm run deploy
```

### 5. Disable auth during development

Add to `wrangler.toml`:
```toml
[vars]
AUTH_DISABLED = "true"
```

**Remove this before production deployment.**

---

## Local Development

```bash
npm install
npx wrangler dev
```

The Worker runs at `http://localhost:8787`. The room Durable Object is backed by a local SQLite store under `.wrangler/`.

---

## Testing & Verification

```bash
# TypeScript type check
npx tsc --noEmit

# Unit tests (51 tests covering room logic, router, long-poll, T8)
npx vitest run

# Dry-run build (no Cloudflare login required)
npx wrangler deploy --dry-run
```

---

## Host (Go + Pion) Integration Example

```go
// 1. Fetch TURN credentials
resp, _ := http.Post(signalingURL+"/turn/credentials", "application/json", nil)
var turnCreds TurnCredentialsResponse
json.NewDecoder(resp.Body).Decode(&turnCreds)

// 2. Post SDP offer
sdpOffer, _ := json.Marshal(map[string]string{"type": "offer", "sdp": pc.LocalDescription().SDP})
http.Post(signalingURL+"/rooms/"+roomID+"/host", "application/json", bytes.NewReader(sdpOffer))

// 3. Long-poll for the viewer's answer / ICE (wait=25 → no busy loop).
//    Or open GET /rooms/{roomID}/host/ws for push instead.
since := -1
for {
    r, _ := http.Get(fmt.Sprintf("%s/rooms/%s/host?since=%d&wait=25", signalingURL, roomID, since))
    var msgs MessagesResponse
    json.NewDecoder(r.Body).Decode(&msgs)
    for _, m := range msgs.Messages {
        // handle SDP answer or ICE candidate
    }
    since = msgs.NextSince
    // wait=25 already blocked up to 25s server-side; reconnect immediately.
}
```

---

## Required Secrets Summary

| Secret | Required | Description |
|---|---|---|
| `TURN_KEY_ID` | Yes (for T8) | Cloudflare Realtime TURN key ID |
| `TURN_KEY_API_TOKEN` | Yes (for T8) | Cloudflare Realtime TURN API token |
| `SESSION_SECRET` | No | Enables stub HMAC token validation |

---

## File Structure

```
playgate-signaling/
├── src/
│   ├── index.ts          # Worker entry: auth/CORS/turn/healthz + routes /rooms/* to the DO
│   ├── roomDO.ts         # RoomDO Durable Object: HTTP + long-poll + WebSocket hibernation glue
│   ├── roomState.ts      # Pure room/queue logic (append, offer-reset, since-filter, seq) — unit-tested
│   ├── types.ts          # Shared TypeScript types + Env bindings (ROOMS: DurableObjectNamespace)
│   ├── cors.ts           # CORS headers + response helpers
│   ├── auth.ts           # Auth stub (SESSION_SECRET / AUTH_DISABLED)
│   ├── turn.ts           # T8 TURN credential endpoint
│   └── __tests__/
│       ├── helpers.ts          # Fake DO state/namespace + request factories
│       ├── roomState.test.ts   # 15 tests for the pure room logic
│       ├── rooms.test.ts       # 27 tests for the router + long-poll
│       └── turn.test.ts        # 9 tests for T8
├── wrangler.toml         # Worker config + DO binding + sqlite migration
├── tsconfig.json
├── vitest.config.ts
└── package.json
```
