# playgate-signaling

Cloudflare Workers-based WebRTC signaling server for PlayGate.

Implements **T7** (SDP offer/answer + ICE candidate exchange via Workers KV) and **T8** (short-lived Cloudflare Realtime TURN credential issuance).

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
- Appends the payload to the sender's KV queue with a 5-minute TTL.

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
GET /rooms/{roomId}/{peer}?since=<lastSeq>
Authorization: Bearer <session-token>
```

Returns messages posted by the **other** peer.  
`since` (optional): last `seq` the caller has already processed; messages with `seq > since` are returned.  
Omit `since` (or pass `-1`) to receive all messages.

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

**Example — viewer polls for host's offer:**
```bash
curl "https://<worker>.workers.dev/rooms/my-room/viewer?since=-1"
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

### 2. Create a KV namespace

```bash
npx wrangler kv namespace create SIGNALING_KV
# Note the returned id

npx wrangler kv namespace create SIGNALING_KV --preview
# Note the returned preview_id
```

Edit `wrangler.toml` and replace the placeholder values:
```toml
[[kv_namespaces]]
binding = "SIGNALING_KV"
id = "<your-kv-namespace-id>"
preview_id = "<your-kv-preview-namespace-id>"
```

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

The Worker runs at `http://localhost:8787`. KV is backed by a local SQLite store.

---

## Testing & Verification

```bash
# TypeScript type check
npx tsc --noEmit

# Unit tests (27 tests covering T7 + T8)
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

// 3. Poll for viewer's answer
since := -1
for {
    r, _ := http.Get(fmt.Sprintf("%s/rooms/%s/host?since=%d", signalingURL, roomID, since))
    var msgs MessagesResponse
    json.NewDecoder(r.Body).Decode(&msgs)
    for _, m := range msgs.Messages {
        // handle SDP answer or ICE candidate
    }
    since = msgs.NextSince
    time.Sleep(500 * time.Millisecond)
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
│   ├── index.ts        # Worker entry point + routing
│   ├── types.ts        # Shared TypeScript types + Env bindings
│   ├── cors.ts         # CORS headers + response helpers
│   ├── auth.ts         # Auth stub (SESSION_SECRET / AUTH_DISABLED)
│   ├── rooms.ts        # T7 signaling endpoints
│   ├── turn.ts         # T8 TURN credential endpoint
│   └── __tests__/
│       ├── helpers.ts       # MockKVNamespace + test factories
│       ├── rooms.test.ts    # 18 tests for T7
│       └── turn.test.ts     # 9 tests for T8
├── wrangler.toml       # Worker config + KV binding
├── tsconfig.json
├── vitest.config.ts
└── package.json
```
