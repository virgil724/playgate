# PlayGate Deployment Guide

This document covers three deployment paths for every PlayGate component:

1. **One-click script** — Raspberry Pi / Debian, simplest path for streamers
2. **Docker** — containerised deployment, suitable for servers or NAS
3. **Manual** — build from source and configure every step yourself

Components to deploy:

| Component | What it does | Where it runs |
|---|---|---|
| `playgate-signaling` | WebRTC signaling + TURN credentials | Cloudflare Workers (free tier) |
| `playgate-server` | REST API: rooms, tokens, session JWTs | Any Linux server / VPS |
| `playgate-host` | Captures video, streams via WebRTC | Raspberry Pi or Linux capture machine |
| `playgate-web` | Viewer browser UI + streamer dashboard | Static hosting (Cloudflare Pages / Netlify / S3) |

---

## Quick-start order

Deploy in this order — each step depends on the previous:

```
1. playgate-signaling  (Cloudflare Workers — no server needed)
2. playgate-server     (VPS or same machine as host)
3. playgate-host       (Raspberry Pi / capture Linux box)
4. playgate-web        (static host; point at server + signaling URLs)
```

---

## 1. playgate-signaling — Cloudflare Workers

### Prerequisites

- Node 18+ and npm
- A [Cloudflare account](https://dash.cloudflare.com/) (free tier is sufficient)
- `wrangler` CLI

```bash
npm install -g wrangler
wrangler login
```

### Step 1 — Create the KV namespace

```bash
cd playgate-signaling
npx wrangler kv namespace create SIGNALING_KV
# Copy the returned id

npx wrangler kv namespace create SIGNALING_KV --preview
# Copy the returned preview_id
```

Edit `wrangler.toml`:

```toml
[[kv_namespaces]]
binding = "SIGNALING_KV"
id = "<your-kv-namespace-id>"
preview_id = "<your-kv-preview-namespace-id>"
```

### Step 2 — Set secrets

```bash
# Cloudflare Realtime TURN (for relay when STUN fails)
# Get these from: Cloudflare Dashboard → Realtime → TURN → Create Key
npx wrangler secret put TURN_KEY_ID
npx wrangler secret put TURN_KEY_API_TOKEN

# Optional: shared-secret stub auth (set before production)
npx wrangler secret put SESSION_SECRET
```

### Step 3 — Deploy

```bash
npm install
npm run deploy
# Wrangler prints the Worker URL, e.g.:
#   https://playgate-signaling.your-subdomain.workers.dev
```

Note this URL — you will need it for `playgate-host` (`signaling.url`) and
`playgate-web` (`VITE_SIGNALING_BASE_URL`).

### Step 4 — Verify

```bash
curl https://playgate-signaling.your-subdomain.workers.dev/healthz
# {"status":"ok"}
```

### Security notes

- Remove `AUTH_DISABLED = "true"` from `wrangler.toml` before going live.
- Set `SESSION_SECRET` to a random 32-byte hex string: `openssl rand -hex 32`
- The Worker URL is public — keep your session secret out of version control.

---

## 2. playgate-server

### Option A — Systemd (recommended for VPS)

#### Build

On the target machine (Linux amd64):

```bash
# From repo root
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go -C playgate-server build -trimpath -ldflags "-s -w" -o /usr/local/bin/playgate-server .
```

Or download a pre-built binary from the GitHub Releases page.

#### Create a data directory

```bash
sudo mkdir -p /var/lib/playgate-server
sudo useradd --system --no-create-home --shell /usr/sbin/nologin playgate-server
sudo chown playgate-server:playgate-server /var/lib/playgate-server
```

#### Create systemd unit

```ini
# /etc/systemd/system/playgate-server.service
[Unit]
Description=PlayGate Server
After=network.target

[Service]
Type=simple
User=playgate-server
ExecStart=/usr/local/bin/playgate-server \
  -addr=:8080 \
  -db=/var/lib/playgate-server/playgate.db \
  -key=/var/lib/playgate-server/ed25519.pem
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now playgate-server
journalctl -u playgate-server -f
```

On first run, the server auto-generates `/var/lib/playgate-server/ed25519.pem`
and prints the base64 public key. Copy it — you will need it for `playgate-host`
(`session.public_key_base64`).

#### Security notes

- Back up `ed25519.pem` immediately. If lost, all outstanding session JWTs
  become unverifiable and you must re-register hosts.
- Place playgate-server behind a reverse proxy (nginx/Caddy) with TLS.
- The API key printed by `POST /api/hosts/register` is a streamer secret —
  treat it like a password.

### Option B — Docker

```bash
docker run -d \
  --name playgate-server \
  --restart unless-stopped \
  -p 8080:8080 \
  -v playgate-server-data:/data \
  -e PLAYGATE_DB=/data/playgate.db \
  -e PLAYGATE_KEY=/data/ed25519.pem \
  ghcr.io/playgate/playgate-server:latest
```

Verify:

```bash
curl http://localhost:8080/api/public-key
```

See `docker/compose.example.yaml` for a full Compose example.

---

## 3. playgate-host (Raspberry Pi / Linux capture machine)

### Option A — One-click install script (recommended)

The script handles everything: system packages, BlueZ reconfiguration, nuxbt
(an actively maintained fork of NXBT; Switch 2 controller support is on its
roadmap, not yet implemented), binary download, user creation, systemd units.

```bash
curl -fsSL https://github.com/playgate/playgate/releases/latest/download/install-host.sh \
  | sudo bash
```

Or, if you have the repo locally:

```bash
sudo bash scripts/install-host.sh
```

After installation:

1. Edit `/etc/playgate/config.yaml` — set `signaling.url` and optionally
   `session.public_key_base64` (from step 2 above).
2. Restart: `sudo systemctl restart playgate-host`
3. Verify: `journalctl -u playgate-host -f`

To uninstall:

```bash
sudo bash scripts/install-host.sh --uninstall
```

#### What the script does

| Step | Action |
|---|---|
| 1 | `apt-get install` ffmpeg, bluez, bluetooth, git, python3-pip, python3-dbus, python3-gi, dbus, v4l-utils |
| 2 | `pip3 install "git+https://github.com/hannahbee91/nuxbt.git@v3.3.6"` |
| 3a | Creates `/etc/systemd/system/bluetooth.service.d/playgate-noplugin.conf` — drops-in `--noplugin=input` onto bluetoothd's ExecStart |
| 3b | Backs up `/etc/bluetooth/main.conf` → `.playgate-bak`, then sets `Experimental = true` and `AutoEnable = true` |
| 3c | `systemctl restart bluetooth` |
| 4 | Creates system user `playgate` in groups `video` + `bluetooth` |
| 5 | Downloads `playgate-host-linux-{arm64\|amd64}` binary to `/usr/local/bin/` |
| 6 | Installs `nxbtd.py` to `/opt/playgate/nxbt-daemon/` |
| 7 | Writes `/etc/playgate/config.yaml` template (if not present) |
| 8 | Installs `nxbtd.service` + `playgate-host.service` to `/etc/systemd/system/` |
| 9 | `systemctl enable` + `start` both services |

#### BlueZ changes in detail

**`/etc/systemd/system/bluetooth.service.d/playgate-noplugin.conf`**

```ini
[Service]
ExecStart=
ExecStart=/usr/lib/bluetooth/bluetoothd --noplugin=input
```

Why: The BlueZ `input` plugin claims HID profiles on startup, which prevents
nuxbt from registering a Nintendo Switch Pro Controller profile. The empty
`ExecStart=` clears the inherited value before the new one is set.

**`/etc/bluetooth/main.conf`** additions:

```ini
[General]
Experimental = true

[Policy]
AutoEnable = true
```

Why: Some BlueZ versions (5.53+) require experimental features enabled for the
DBus interfaces nuxbt uses. `AutoEnable=true` ensures the Bluetooth adapter
powers on automatically after reboot.

The original file is backed up to `/etc/bluetooth/main.conf.playgate-bak`.
`--uninstall` restores this backup.

### Option B — Docker

The Docker image bundles ffmpeg, BlueZ tools, Python, and nuxbt. The container
must run with `--privileged` and `--network host` for Bluetooth and mDNS ICE.

**Requirements on the host machine:**

- BlueZ must be running on the host (not inside the container)
- `/var/run/dbus` must be the host DBus socket (nuxbt talks to it)
- The BlueZ `--noplugin=input` change must still be applied to the host's
  bluetoothd (same as Option A, steps 3a–3c)

Apply BlueZ config on the host first:

```bash
# On the Raspberry Pi / capture machine (one-time setup)
sudo bash scripts/install-host.sh --bluez-only   # (not yet implemented — do manually)
```

Or manually:

```bash
sudo mkdir -p /etc/systemd/system/bluetooth.service.d
sudo tee /etc/systemd/system/bluetooth.service.d/playgate-noplugin.conf <<'EOF'
[Service]
ExecStart=
ExecStart=/usr/lib/bluetooth/bluetoothd --noplugin=input
EOF
sudo systemctl daemon-reload
sudo systemctl restart bluetooth
```

Then run the container:

```bash
docker run -d \
  --name playgate-host \
  --restart unless-stopped \
  --privileged \
  --network host \
  --device /dev/video0 \
  -v /var/run/dbus:/var/run/dbus \
  -v /run/nxbt:/run/nxbt \
  -v /etc/playgate:/etc/playgate:ro \
  ghcr.io/playgate/playgate-host:latest
```

See `docker/compose.example.yaml` for a Compose example with both server and host.

### Option C — Manual build and run

```bash
# Build for Raspberry Pi (from any machine with Go installed)
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go -C playgate-host build -trimpath -ldflags "-s -w" \
  -o dist/playgate-host-linux-arm64 ./cmd/host

# SCP to the Pi
scp dist/playgate-host-linux-arm64 pi@raspberrypi:/usr/local/bin/playgate-host
scp playgate-host/config.example.yaml pi@raspberrypi:/etc/playgate/config.yaml

# On the Pi: apply BlueZ changes, install nxbt, place nxbtd.py, then:
sudo /usr/bin/python3 /opt/playgate/nxbt-daemon/nxbtd.py &
sudo playgate-host -config /etc/playgate/config.yaml
```

### libav build (optional, in-process encoder)

The default host binary is pure Go (`CGO_ENABLED=0`) and shells out to the
`ffmpeg` CLI. The optional **libav** build (`-tags libav`, cgo + go-astiav)
encodes in-process, enabling live bitrate changes and cheap keyframes. Select it
at runtime with `encoder.backend: libav` (default is `cli`); a non-libav binary
rejects `libav` with an explicit error.

Because it uses cgo it **cannot be cross-compiled**: build each arch natively
(amd64 on an amd64 Linux host, arm64 on an arm64 host such as a Pi), or let Docker
buildx do it per platform (QEMU for the foreign arch).

FFmpeg version: CI and the Docker image both use **Debian trixie = FFmpeg 7.1**
(libavcodec61 / libavformat61 / libavutil59 / libswscale8 / libswresample5 /
libavfilter10 / libavdevice61). go-astiav v0.39.0 targets the FFmpeg 7.x C API
(its README says n7.0; 7.1 has the same library majors) and is used with the
7.1 libs. Debian bookworm (5.1) and Ubuntu 24.04 (6.1) are too old. The binary
links FFmpeg dynamically, so the target machine needs the matching runtime libs.

```bash
# Native build (Debian 13 trixie or another FFmpeg 7.1 distro); dev packages needed once:
sudo apt-get install -y gcc pkg-config libavcodec-dev libavformat-dev \
  libavutil-dev libswscale-dev libavfilter-dev libavdevice-dev libswresample-dev
make build-libav-amd64      # on amd64  -> dist/playgate-host-libav-linux-amd64
make build-libav-arm64      # on arm64  -> dist/playgate-host-libav-linux-arm64

# Docker (multi-arch, each platform built natively under buildx/QEMU):
docker buildx build --platform linux/amd64,linux/arm64 \
  --target host-libav -f docker/host.Dockerfile \
  -t ghcr.io/playgate/playgate-host:libav --push .
# or: make docker-host-libav REGISTRY=ghcr.io/your-org/playgate
```

#### Fully static libav release (primary distributable, manual)

The dynamic build above needs FFmpeg 7.1 runtime libs on the target, which
Raspberry Pi OS bookworm (FFmpeg 5.1) and Ubuntu 24.04 (6.1) do not have. For a
self-contained binary, run the manually triggered **release-libav-static**
workflow; it is separate from `ci.yml` / `release.yml`, which stay pure-Go:

```bash
gh workflow run release-libav-static.yml -f tag=v0.1.0   # tag must already have a release
# optional: -f ffmpeg_version=7.1.5  -f ref=<git ref of source to build>
```

It builds on native amd64 and arm64 runners inside Alpine (musl): x264 and
FFmpeg 7.1.x from source as static libs (cached per arch/version/script hash),
then `CGO_ENABLED=1 go build -tags libav,netgo,osusergo` with
`-linkmode external -extldflags "-static"`. It runs the libav tests, checks
`file`/`ldd` report a static binary, smoke-runs it on `debian:bookworm-slim`, and
uploads `playgate-host-libav-static-linux-<arch>`, a `.sha256`, and a
`.BUILDINFO.txt` (exact FFmpeg/x264 versions, source SHA256s, configure lines)
to the release. The same steps run locally via `scripts/build-ffmpeg-static.sh`
and `scripts/build-host-libav-static.sh` in an `alpine:3.20` container.

| Feature | Static build |
|---------|--------------|
| libx264 software encode | yes (statically linked, GPL) |
| `h264_v4l2m2m` (Pi 4 hardware encoder) | compiled in (needs kernel V4L2 M2M device; untested on hardware) |
| NVENC (`h264_nvenc`) | **not available** - NVENC `dlopen()`s the NVIDIA driver at runtime, which a static musl binary cannot do (and a glibc driver cannot be loaded into a musl process). Use the dynamic glibc libav build or the CLI backend for NVENC. |
| VAAPI | **not available** - needs dynamic libva/driver. Use the dynamic build. |

**GPL notice.** Statically linking x264 and building FFmpeg with `--enable-gpl`
makes the distributed static binary a GPL work (not just a program that happens
to call GPL libraries). Anyone you give the binary to must be offered the
corresponding source: the playgate-host source at the release tag, the
unmodified FFmpeg and x264 sources named in `.BUILDINFO.txt`, and the build
scripts above. Keep that file attached to the release and link it from
distribution pages. If GPL is unacceptable, build FFmpeg with OpenH264 (BSD,
software H.264) plus NVENC (no `--enable-gpl`, no libx264) instead; that
variant is not produced by the workflow today.

---

## 4. playgate-web — Static hosting

playgate-web is a Vite/React SPA. Build it once and serve the `dist/` directory
from any static host (Cloudflare Pages, Netlify, GitHub Pages, nginx, S3+CloudFront).

### Build

```bash
cd playgate-web

# 1. Create .env.local with your deployed URLs:
cat > .env.local <<EOF
VITE_API_BASE_URL=https://api.your-domain.com       # playgate-server
VITE_SIGNALING_BASE_URL=https://signaling.workers.dev  # playgate-signaling Worker
EOF

# 2. Install and build
npm install
npm run build
# Output: playgate-web/dist/
```

### Deploy to Cloudflare Pages

```bash
npx wrangler pages deploy dist --project-name playgate-web
```

Or connect your GitHub repo to Cloudflare Pages (Settings → Build command: `npm run build`, output: `dist`).

### Deploy to Netlify

```bash
npm install -g netlify-cli
netlify deploy --prod --dir=dist
```

### Environment variables

| Variable | Description |
|---|---|
| `VITE_API_BASE_URL` | Full URL to playgate-server REST API (e.g. `https://api.example.com`) |
| `VITE_SIGNALING_BASE_URL` | Full URL to playgate-signaling Worker (e.g. `https://xyz.workers.dev`) |

These are baked in at build time. If the URLs change, rebuild and redeploy.

### Security notes

- Serve `dist/` over HTTPS only — WebRTC requires a secure context.
- Set CORS on playgate-server to allow your frontend domain (the server currently
  sends `Access-Control-Allow-Origin: *`, which is fine for public APIs).

---

## Service dependency order

```
bluetoothd (host OS)
    └── nxbtd.service
            └── playgate-host.service
                    └── (connects to playgate-signaling Worker)
                    └── (optionally verifies JWTs via playgate-server public key)
```

Systemd handles the ordering automatically via `Requires=` and `After=` in the
unit files (`scripts/systemd/`).

---

## Build reference (Makefile / build.ps1)

```bash
# Linux / macOS (requires make)
make host-linux-arm64      # → dist/playgate-host-linux-arm64
make host-linux-amd64      # → dist/playgate-host-linux-amd64
make server-linux-amd64    # → dist/playgate-server-linux-amd64
make server-linux-arm64    # → dist/playgate-server-linux-arm64
make all                   # build all four
make test                  # Go tests + Vitest
make docker                # multi-arch Docker buildx (set REGISTRY=)
make clean                 # remove dist/
```

```powershell
# Windows (PowerShell, no make required)
.\build.ps1                              # all four binaries
.\build.ps1 -Target host-linux-arm64    # single target
.\build.ps1 -Target test                # run tests
.\build.ps1 -Target clean
```

---

## Secrets and key management

| Secret | Location | Risk if lost |
|---|---|---|
| `ed25519.pem` (server) | `/var/lib/playgate-server/` or `/data/` in Docker | All outstanding session JWTs unverifiable; re-register all hosts |
| `api_key` (per host) | Returned once by `POST /api/hosts/register` | Attacker can create rooms and issue tokens as that host |
| `SESSION_SECRET` (signaling) | Cloudflare Worker secret | Attackers can forge signaling auth tokens |
| `TURN_KEY_API_TOKEN` | Cloudflare Worker secret | Attacker can generate TURN credentials at your expense |

**Backup checklist:**

- [ ] Copy `ed25519.pem` off the server immediately after first run
- [ ] Store `api_key` in a password manager
- [ ] Rotate `SESSION_SECRET` if the Cloudflare Worker is compromised
- [ ] Enable Cloudflare API token restrictions (IP allow-list, zone-scoped)

---

## Updating

### playgate-host binary

```bash
sudo systemctl stop playgate-host
sudo curl -fsSL <release-url>/playgate-host-linux-arm64 \
  -o /usr/local/bin/playgate-host
sudo chmod +x /usr/local/bin/playgate-host
sudo systemctl start playgate-host
```

### playgate-server Docker

```bash
docker pull ghcr.io/playgate/playgate-server:latest
docker compose up -d server   # or docker stop/rm + docker run
```

The SQLite database and key survive in the named volume `playgate-server-data`.

### playgate-signaling

```bash
cd playgate-signaling
npm install
npm run deploy   # zero-downtime rollout via Cloudflare
```

---

## Troubleshooting

### Host does not appear online

1. Check `journalctl -u playgate-host -f` for errors.
2. Confirm `signaling.url` in `/etc/playgate/config.yaml` matches the Worker URL.
3. Test the Worker: `curl https://<worker>/healthz`

### nuxbt fails with "DBus connection error"

- Ensure `bluetoothd` is running: `systemctl status bluetooth`
- Confirm `--noplugin=input` is active: `systemctl cat bluetooth | grep ExecStart`
- Verify `/var/run/dbus/system_bus_socket` exists (DBus daemon must be running)

### No video from capture card

- List devices: `v4l2-ctl --list-devices`
- Check the `playgate` user is in the `video` group: `groups playgate`
- Confirm the device path in `config.yaml` matches (e.g. `/dev/video0`)

### Bluetooth controller not pairing

- After first start of nuxbt, pair the Switch: hold Sync button on the controller
  while `nxbtd.service` is running.
- Check logs: `journalctl -u nxbtd -f`
- If the daemon is in mock mode (nuxbt not importable / no BlueZ), the logs
  will say "mock mode".
