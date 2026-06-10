# docker/server.Dockerfile — PlayGate Server
# ─────────────────────────────────────────────────────────────────────────────
# Builds a minimal, static playgate-server image using distroless/static.
# modernc.org/sqlite is pure Go — no CGO, no libc dependency.
#
# Build (multi-arch, from repo root):
#   docker buildx build \
#     --platform linux/amd64,linux/arm64 \
#     --file docker/server.Dockerfile \
#     --tag ghcr.io/playgate/playgate-server:latest \
#     --push .
#
# Run:
#   docker run -d \
#     --name playgate-server \
#     --restart unless-stopped \
#     -p 8080:8080 \
#     -v playgate-data:/data \
#     -e PLAYGATE_DB=/data/playgate.db \
#     -e PLAYGATE_KEY=/data/ed25519.pem \
#     ghcr.io/playgate/playgate-server:latest
#
# Volumes:
#   /data — SQLite database + ed25519 key (must be a persistent volume)
# ─────────────────────────────────────────────────────────────────────────────

# ── Stage 1: builder ─────────────────────────────────────────────────────────
FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS builder

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

WORKDIR /src

COPY playgate-server/go.mod playgate-server/go.sum ./playgate-server/
RUN go -C playgate-server mod download

COPY playgate-server/ ./playgate-server/

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go -C playgate-server build \
      -trimpath \
      -ldflags "-s -w \
        -X main.Version=${VERSION} \
        -X main.Commit=${COMMIT} \
        -X main.BuildDate=${BUILD_DATE}" \
      -o /out/playgate-server \
      .

# ── Stage 2: distroless runtime ──────────────────────────────────────────────
# gcr.io/distroless/static-debian12 has no shell, no package manager,
# only CA certs and tzdata — perfect for a pure-Go static binary.
FROM gcr.io/distroless/static-debian12 AS runtime

COPY --from=builder /out/playgate-server /playgate-server

# Data volume: database + key are stored here
VOLUME ["/data"]

ENV PLAYGATE_ADDR=:8080
ENV PLAYGATE_DB=/data/playgate.db
ENV PLAYGATE_KEY=/data/ed25519.pem

EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/playgate-server"]
