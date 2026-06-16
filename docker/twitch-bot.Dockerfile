# docker/twitch-bot.Dockerfile — PlayGate Twitch Bot
# ─────────────────────────────────────────────────────────────────────────────
# Standalone Node service that auto-distributes PlayGate codes to Twitch viewers.
# Multi-stage: compile TypeScript → run on a slim Node runtime as a nonroot user.
#
# Build (from repo root):
#   docker buildx build \
#     --platform linux/amd64,linux/arm64 \
#     --file docker/twitch-bot.Dockerfile \
#     --tag ghcr.io/virgil724/playgate-twitch-bot:latest \
#     --push .
#
# Run: see docker/compose.twitch-bot.yaml.
#
# Working dir is /data (a persistent volume): it holds config.yaml plus the
# grants/token state files. The admin page binds 0.0.0.0 inside the container
# and is meant to be published ONLY to localhost (see the compose file).
# ─────────────────────────────────────────────────────────────────────────────

# ── Stage 1: builder ─────────────────────────────────────────────────────────
FROM node:22-bookworm-slim AS builder
WORKDIR /app

COPY playgate-twitch-bot/package.json playgate-twitch-bot/package-lock.json ./
RUN npm ci

COPY playgate-twitch-bot/tsconfig.json ./
COPY playgate-twitch-bot/src ./src
# Compile, then drop compiled test files (build context is the repo root, so the
# package .dockerignore doesn't apply — strip them here instead).
RUN npm run build && find dist -name '*.test.js' -delete

# ── Stage 2: runtime ─────────────────────────────────────────────────────────
FROM node:22-bookworm-slim AS runtime
ENV NODE_ENV=production
WORKDIR /app

COPY playgate-twitch-bot/package.json playgate-twitch-bot/package-lock.json ./
RUN npm ci --omit=dev && npm cache clean --force

# Compiled JS + the admin static assets (tsc doesn't copy non-.ts files).
COPY --from=builder /app/dist ./dist
COPY playgate-twitch-bot/src/admin/public ./dist/admin/public
# Seed config used by the compose init step to populate an empty data volume.
COPY playgate-twitch-bot/config.example.yaml ./config.example.yaml

# /data holds config.yaml + state; chown so the nonroot 'node' user can write.
RUN mkdir -p /data && chown node:node /data
WORKDIR /data
USER node

ENV ADMIN_BIND=0.0.0.0
EXPOSE 8090

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD node -e "fetch('http://127.0.0.1:'+(process.env.ADMIN_PORT||8090)+'/api/status').then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))"

ENTRYPOINT ["node", "/app/dist/index.js"]
