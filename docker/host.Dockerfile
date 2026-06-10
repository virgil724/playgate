# docker/host.Dockerfile — PlayGate Host
# ─────────────────────────────────────────────────────────────────────────────
# Multi-stage build:
#   Stage 1 (builder)  — cross-compile playgate-host for the target platform
#   Stage 2 (runtime)  — Debian-slim with ffmpeg, BlueZ, Python3 + nxbt
#
# Build (multi-arch, from repo root):
#   docker buildx build \
#     --platform linux/amd64,linux/arm64 \
#     --file docker/host.Dockerfile \
#     --tag ghcr.io/playgate/playgate-host:latest \
#     --push .
#
# Run (Raspberry Pi example):
#   docker run -d \
#     --name playgate-host \
#     --restart unless-stopped \
#     --privileged \
#     --network host \
#     --device /dev/video0 \
#     -v /var/run/dbus:/var/run/dbus \
#     -v /run/nxbt:/run/nxbt \
#     -v /etc/playgate:/etc/playgate:ro \
#     ghcr.io/playgate/playgate-host:latest
#
# Runtime requirements:
#   --privileged or --cap-add NET_ADMIN,SYS_ADMIN  → nxbt needs DBus/BlueZ
#   --network host                                  → mDNS ICE candidates
#   --device /dev/video0                            → V4L2 capture card
#   -v /var/run/dbus:/var/run/dbus                  → bluetoothd IPC
#   -v /run/nxbt:/run/nxbt                          → shared UNIX socket dir
#   -v /etc/playgate:/etc/playgate:ro               → mount config.yaml
# ─────────────────────────────────────────────────────────────────────────────

# ── Stage 1: builder ─────────────────────────────────────────────────────────
FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS builder

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

WORKDIR /src

# Cache module downloads separately from source
COPY playgate-host/go.mod playgate-host/go.sum ./playgate-host/
RUN go -C playgate-host mod download

# Copy source
COPY playgate-host/ ./playgate-host/

# Cross-compile — pure Go, no CGO required
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go -C playgate-host build \
      -trimpath \
      -ldflags "-s -w \
        -X main.Version=${VERSION} \
        -X main.Commit=${COMMIT} \
        -X main.BuildDate=${BUILD_DATE}" \
      -o /out/playgate-host \
      ./cmd/host

# Copy nxbt-daemon Python source
COPY playgate-host/nxbt-daemon/ /out/nxbt-daemon/

# ── Stage 2: runtime ─────────────────────────────────────────────────────────
# debian:bookworm-slim gives a known-good base with apt.
# We install:
#   ffmpeg         — H.264 encoding via ffmpeg subprocess (encoder/ffmpeg)
#   bluez          — bluetoothd for nxbt Pro Controller emulation
#   python3-pip    — to install nxbt Python library
#   python3-dbus   — DBus bindings needed by nxbt
#   dbus           — dbus-daemon (may already be on host, but needed in image)
#   v4l-utils      — optional, useful for /dev/video0 debugging
FROM debian:bookworm-slim AS runtime

# Avoid interactive prompts during apt
ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates \
      ffmpeg \
      bluez \
      bluetooth \
      python3 \
      python3-pip \
      python3-dbus \
      python3-gi \
      dbus \
      v4l-utils \
    && rm -rf /var/lib/apt/lists/*

# Install nxbt (Linux-only Bluetooth Pro Controller library)
# nxbt>=0.1.5 requires BlueZ, DBus, and the uhid kernel module on the HOST.
RUN pip3 install --no-cache-dir --break-system-packages "nxbt>=0.1.5"

# Copy compiled binary + daemon
COPY --from=builder /out/playgate-host /usr/local/bin/playgate-host
COPY --from=builder /out/nxbt-daemon   /opt/playgate/nxbt-daemon

# Runtime directory for UNIX socket (host also uses /run/nxbt by default)
RUN mkdir -p /run/nxbt /etc/playgate

# Config placeholder — mount a real config.yaml at /etc/playgate/config.yaml
COPY playgate-host/config.example.yaml /etc/playgate/config.example.yaml

# Entrypoint: start nxbt daemon in background, then run the host binary
# The entrypoint script waits for the socket to appear before launching host.
COPY docker/host-entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
