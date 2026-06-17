# playgate-twitch-bot

Automatically distributes PlayGate token codes to Twitch viewers. Watches several
trigger sources (chat command, Channel Points redemption, subscription / cheer /
raid events), applies a configurable policy, and delivers each code by **private
whisper** (with a public chat fallback).

It is a standalone client of `playgate-server` — it only calls the existing
`POST /api/rooms/{id}/tokens` endpoint. The server is unchanged; codes are still
issued **by the streamer's host account**, matching PlayGate's SaaS model.

## How it works

```
 [Twitch chat IRC]  ─┐
 [Channel Points]   ─┼─►  GrantRequest  ─►  Policy  ─►  TokenPool  ─►  Whisper (→ chat fallback)
 [sub / cheer / raid]┘    (unified)         (eligible? (pre-minted        (private delivery)
                                             cooldown?  code buffer)
                                             rate/cap?)
```

Every source is normalised into one `GrantRequest` and funnelled through a single
pipeline (`src/grant/pipeline.ts`), so adding a source never touches policy or
delivery.

## Prerequisites

1. **A running playgate-server** with a registered host and a room:
   - `POST /api/hosts/register` → save the `api_key`.
   - `POST /api/rooms` (with that key) → save the `id`.
2. **A Twitch application** (https://dev.twitch.tv/console):
   - Note the **Client ID** and **Client Secret**.
   - Add `http://localhost:8090/callback` as an **OAuth Redirect URL**
     (match the `admin.port` in `config.yaml`).
3. **A bot Twitch account** with a **verified phone number** — Twitch requires
   this to send whispers via the API.

## Configure

```sh
cp .env.example .env            # secrets
cp config.example.yaml config.yaml  # tuning + policy
```

`.env` — secrets only:

```
PLAYGATE_HOST_API_KEY=...   # from /api/hosts/register
PLAYGATE_ROOM_ID=...        # the room to issue codes for
TWITCH_CLIENT_ID=...
TWITCH_CLIENT_SECRET=...
```

`config.yaml` — non-secret tuning. The `policy` block is also editable from the
admin page (which writes it back here). Set `playgate.webBase` to your deployed
playgate-web origin so redeem links resolve.

## Run

```sh
npm install
npm start        # or: npm run dev (watch mode)
```

On first run open **http://localhost:8090** and click **Connect Broadcaster**
and **Connect Bot** to grant OAuth. The Twitch sources start automatically once
both are connected — no restart needed. Tokens persist to `twitch.tokens.json`,
so you only authorize once.

The chat channel and bot connect-identity are taken from the authorized accounts
(broadcaster's login = channel, bot's login = identity), so there is nothing else
to set. Override only if needed via `twitch.channelLogin` / `twitch.botLogin` in
config.yaml (or `TWITCH_CHANNEL_LOGIN` / `TWITCH_BOT_LOGIN`).

## Docker

A multi-stage image and a standalone compose stack are provided. The image is
built and pushed to GHCR by CI (`.github/workflows/docker-twitch-bot.yml`) on
every change to `main`; `ci.yml` also build-smokes the Dockerfile on PRs.

```sh
# from the docker/ directory, create .env with your secrets/IDs (see the
# header of compose.twitch-bot.yaml), then:
docker compose -f compose.twitch-bot.yaml up -d
# open http://localhost:8090 and connect Broadcaster + Bot
```

Notes specific to containers:

- **config + state live in the `bot-data` volume** (`config.yaml`,
  `twitch.tokens.json`, `grants.state.json`). An init step seeds `config.yaml`
  from the bundled example on first run; the policy editor writes back into the
  volume. Back this volume up.
- **The admin page binds `0.0.0.0` inside the container** (`ADMIN_BIND`) but is
  published only to `127.0.0.1:8090`, so it stays off the network.
- **Identifiers can come from the environment** (`PLAYGATE_API_BASE`,
  `PLAYGATE_WEB_BASE`, `TWITCH_CHANNEL_LOGIN`, `TWITCH_BOT_LOGIN`, `ADMIN_PORT`),
  overriding `config.yaml` — so the container is configured entirely via env.
- Add `http://localhost:8090/callback` to the Twitch app's OAuth Redirect URLs.

## The admin page (http://localhost:8090)

- **Status** — codes in the buffer, grants this stream, whisper/fallback/failed/
  denied counters, recent grant log, and a "Start new stream session" button
  (resets the per-stream cap).
- **Policy editor** — a form over the whole policy; **Save** validates, writes
  `config.yaml`, and hot-swaps it live.

## Policy

Three layers, evaluated in order (`src/grant/policy.ts`):

1. **Eligibility** — per source. Chat command checks badges
   (`everyone | subscribers | vips | mods`; higher roles satisfy lower tiers).
   Channel Points matches a reward id. Cheer needs `minBits`; raid needs
   `minViewers`. Sub/cheer/raid events are their own qualification.
2. **Cooldown** — one code per Twitch user per `perUserCooldownSec`
   (per-source override falls back to the global value). The main anti-farming
   control.
3. **Rate / cap** — `maxPerMinute` smooths pace; `maxPerStream` caps a session.

State backing cooldown/rate/cap persists to `grants.state.json`, so limits
survive a restart.

## ⚠️ Whisper limitations

Twitch heavily rate-limits `POST /helix/whispers`. Plan around it:

- The sending bot account **must have a verified phone number**.
- Roughly **1 whisper/sec**, ~3/min to never-contacted users, and ~40 unique
  recipients/day. Busy streams will hit this.
- Whispers fail if the recipient blocks messages from strangers.

The bot serializes + spaces whispers and, on failure, **falls back to a public
`@mention` + redeem link**. That link embeds the code, so anyone could redeem it
first — keep a per-user cooldown on. For high-volume channels, Channel Points
redemption + whisper (or a web overlay) is the more reliable pattern than
whispering every chatter.

## Development

```sh
npm run typecheck
npm test          # vitest: policy + token pool
```

## Layout

| Path | Role |
|------|------|
| `src/index.ts` | composition root |
| `src/config.ts` | config + policy schema (zod), load/save |
| `src/grant/` | `GrantRequest`, policy engine, pipeline, message text |
| `src/playgate/` | server API client + pre-minting token pool |
| `src/twitch/` | OAuth, whisper delivery, EventSub WebSocket |
| `src/sources/` | chat command, channel points, sub/cheer/raid adapters |
| `src/store/` | persistent grant history (cooldown/rate state) |
| `src/admin/` | local web admin page + stats |
