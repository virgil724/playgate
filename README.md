# PlayGate

> **DRAFT** — This document reflects the current development state. Review before public release.

PlayGate is an **interactive streaming tool** that lets live-stream viewers take timed, token-gated control of a streamer's game console (Nintendo Switch; PC support planned). It is infrastructure for streamers — analogous to OBS or Parsec — not a game operator or game publisher.

<p align="center">
  <a href="brag-output-2026-06-24-180000/brag.mp4">
    <img src="https://img.shields.io/badge/▶_Watch_the_launch_video-4f8cff?style=for-the-badge&logo=play&logoColor=white" alt="Watch the launch video" />
  </a>
</p>

> 🎬 **[Watch the launch video](brag-output-2026-06-24-180000/brag.mp4)** — 20s overview of the full viewer flow: Twitch chat → token whisper → redeem → gamepad control → live Switch gameplay.

---

## What PlayGate Is (and Is Not)

| It is... | It is not... |
|---|---|
| A SaaS tool sold to streamers (monthly subscription) | A game, game service, or game platform |
| A viewer-interaction layer on top of a streamer's existing setup | A reseller of game content or game licenses |
| Infrastructure that forwards controller input the streamer authorises | An operator of any console or game server |

The streamer retains full control at all times. PlayGate provides the plumbing; the streamer owns the broadcast, the console, and the audience relationship.

---

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                          PlayGate Monorepo                      │
│                                                                 │
│  ┌──────────────────┐          ┌───────────────────────────┐   │
│  │  playgate-host   │          │      playgate-server      │   │
│  │  (Go daemon)     │◄─────────│  (REST API / SaaS backend)│   │
│  │                  │  JWT     │                           │   │
│  │  Runs on the     │  verify  │  Host registration        │   │
│  │  streamer's      │          │  Room management          │   │
│  │  Linux machine   │          │  Token issuance           │   │
│  │                  │          │  Session queue            │   │
│  │  v4l2 capture    │          └───────────────────────────┘   │
│  │  H.264 encode    │                      ▲                   │
│  │  WebRTC publish  │                      │ REST              │
│  │  NXBT controller │          ┌───────────┴───────────────┐   │
│  └────────┬─────────┘          │      playgate-web         │   │
│           │ WebRTC             │  (Viewer browser client)  │   │
│           │ (video + data ch.) │                           │   │
│  ┌────────▼─────────┐          │  Token redemption UI      │   │
│  │playgate-signaling│          │  Queue position display   │   │
│  │(CF Workers / WS) │◄─────────│  WebRTC viewer / control  │   │
│  │                  │  signal  │  Streamer dashboard       │   │
│  │  WebRTC SDP/ICE  │          └───────────────────────────┘   │
│  │  exchange        │                                          │
│  │  TURN credential │                                          │
│  │  (CF Realtime)   │                                          │
│  └──────────────────┘                                          │
└─────────────────────────────────────────────────────────────────┘

Token flow: streamer issues tokens → distributes to viewers (e.g. via chat)
            → viewer redeems token → receives timed session JWT
            → JWT authorises one WebRTC control session
```

---

## Sub-projects

| Directory | Language / Runtime | Role |
|---|---|---|
| [`playgate-host/`](./playgate-host/README.md) | Go (Linux) | Daemon running on the streamer's machine: captures video, encodes H.264, runs WebRTC peer, bridges to NXBT controller daemon |
| [`playgate-server/`](./playgate-server/README.md) | Go | SaaS backend REST API: host registration, room management, token issuance, JWT signing, session queue |
| [`playgate-web/`](./playgate-web/) | TypeScript / Vite | Browser client for viewers (token redemption, queue, WebRTC control) and streamer dashboard |
| [`playgate-signaling/`](./playgate-signaling/README.md) | TypeScript / Cloudflare Workers | WebRTC signaling server (SDP/ICE exchange); TURN credentials via Cloudflare Realtime |
| [`playgate-twitch-bot/`](./playgate-twitch-bot/README.md) | TypeScript / Node | Twitch integration — watches chat commands, Channel Points, and events; distributes token codes via whisper with chat fallback |

---

## Quick Start

Deployment instructions — including infrastructure requirements, environment variables, Docker setup, and step-by-step bring-up — are in **[docs/deploy.md](./docs/deploy.md)**.

> `docs/deploy.md` is maintained by the infrastructure team. If that file does not yet exist in your branch, check the latest `main`.

---

## Legal / Disclaimer

**PlayGate is a streaming interaction tool. It is not affiliated with, endorsed by, or sponsored by Nintendo, Sony, Microsoft, or any game publisher or console manufacturer.**

### Tool-provider positioning

PlayGate is infrastructure for content creators, in the same category as OBS Studio, Parsec, or StreamElements. It does not:

- Distribute, resell, or sublicense any game or game content.
- Operate any game server or game service.
- Sell tokens, game access, or any game-related entitlements to viewers. Tokens are issued exclusively by the streamer (the PlayGate subscriber) and distributed to their own audience at the streamer's discretion.

### User responsibility

**Streamers** using PlayGate are solely responsible for:

- Compliance with the End User License Agreements (EULAs) and Terms of Service of any game, console, or streaming platform they use in conjunction with PlayGate.
- Their channel content, audience interactions, and any obligations arising from their streaming activities.
- Ensuring their use of PlayGate complies with applicable local laws.

PlayGate provides tools; it does not provide legal advice. Nothing in this repository or in PlayGate's documentation constitutes legal advice. Consult qualified legal counsel before using PlayGate in a commercial context.

### DMCA / Copyright

PlayGate maintains a designated DMCA agent and will respond to valid takedown notices. See [docs/compliance-checklist.md](./docs/compliance-checklist.md) for the contact and process.

### Trademarks

Product names, trademarks, and logos of third parties (including but not limited to Nintendo, Switch, PlayStation, Xbox) are the property of their respective owners. Their appearance in any functional description (e.g. "works with your console") does not imply endorsement or affiliation.

---

## Contributing

This is a private monorepo. For internal contribution guidelines, see each sub-project's README.

---

*For the full compliance and risk documentation, see [docs/compliance-checklist.md](./docs/compliance-checklist.md) and [docs/marketing-guidelines.md](./docs/marketing-guidelines.md).*
