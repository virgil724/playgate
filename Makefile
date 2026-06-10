# PlayGate Monorepo Makefile
# ─────────────────────────────────────────────────────────────────────────────
# Usage (Linux / macOS / WSL):
#   make host-linux-arm64        build playgate-host for Raspberry Pi
#   make host-linux-amd64        build playgate-host for x86-64 Linux
#   make server-linux-amd64      build playgate-server for x86-64 Linux
#   make server-linux-arm64      build playgate-server for Raspberry Pi
#   make all                     build all four binaries
#   make test                    run Go + Vitest suites
#   make docker                  multi-arch Docker buildx push (requires REGISTRY)
#   make clean                   remove dist/
#
# On Windows (no make): use build.ps1 instead (see below).
# ─────────────────────────────────────────────────────────────────────────────

# ── Version injection ────────────────────────────────────────────────────────
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo "unknown")

# Common ldflags: strip debug info + inject version
# The version vars are injected as build-time constants via -X flags.
# Adjust the package paths if the main packages expose version vars in future.
LDFLAGS   := -s -w \
             -X main.Version=$(VERSION) \
             -X main.Commit=$(COMMIT) \
             -X main.BuildDate=$(BUILD_DATE)

# ── Directories ──────────────────────────────────────────────────────────────
DIST      := dist
HOST_DIR  := playgate-host
SRV_DIR   := playgate-server
WEB_DIR   := playgate-web

.PHONY: all clean test \
        host-linux-arm64 host-linux-amd64 \
        server-linux-amd64 server-linux-arm64 \
        web docker docker-host docker-server

# ── Default target ───────────────────────────────────────────────────────────
all: host-linux-arm64 host-linux-amd64 server-linux-amd64 server-linux-arm64

# ── Host binaries ────────────────────────────────────────────────────────────
# Expected behaviour on Linux: produces ELF arm64/amd64 binary in dist/.
# CGO_ENABLED=0 → pure Go; no libc dependency at link time.

host-linux-arm64: $(DIST)
	@echo "==> host linux/arm64"
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
	  go -C $(HOST_DIR) build \
	    -trimpath \
	    -ldflags "$(LDFLAGS)" \
	    -o ../$(DIST)/playgate-host-linux-arm64 \
	    ./cmd/host

host-linux-amd64: $(DIST)
	@echo "==> host linux/amd64"
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
	  go -C $(HOST_DIR) build \
	    -trimpath \
	    -ldflags "$(LDFLAGS)" \
	    -o ../$(DIST)/playgate-host-linux-amd64 \
	    ./cmd/host

# ── Server binaries ──────────────────────────────────────────────────────────
# modernc.org/sqlite is pure Go (no CGO); static binary works with scratch image.

server-linux-amd64: $(DIST)
	@echo "==> server linux/amd64"
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
	  go -C $(SRV_DIR) build \
	    -trimpath \
	    -ldflags "$(LDFLAGS)" \
	    -o ../$(DIST)/playgate-server-linux-amd64 \
	    .

server-linux-arm64: $(DIST)
	@echo "==> server linux/arm64"
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
	  go -C $(SRV_DIR) build \
	    -trimpath \
	    -ldflags "$(LDFLAGS)" \
	    -o ../$(DIST)/playgate-server-linux-arm64 \
	    .

# ── Tests ────────────────────────────────────────────────────────────────────
# Expected behaviour on Linux:
#   - runs all Go unit tests for both host and server modules
#   - runs Vitest for playgate-web
test:
	@echo "==> go test playgate-host"
	go -C $(HOST_DIR) test ./...
	@echo "==> go test playgate-server"
	go -C $(SRV_DIR) test ./...
	@echo "==> vitest playgate-web"
	cd $(WEB_DIR) && npm run test

# ── Docker (multi-arch) ──────────────────────────────────────────────────────
# Requires: docker buildx, a builder with arm64 emulation (qemu or native node)
# Set REGISTRY=ghcr.io/your-org/playgate before running.
# Expected behaviour on Linux with Docker:
#   - builds and pushes linux/amd64 + linux/arm64 images
#   - docker/host.Dockerfile: multi-stage Go build + ffmpeg+bluez runtime
#   - docker/server.Dockerfile: static binary on gcr.io/distroless/static

REGISTRY ?= ghcr.io/playgate
TAG      ?= $(VERSION)
PLATFORMS := linux/amd64,linux/arm64

docker: docker-host docker-server

docker-host:
	@echo "==> docker buildx host ($(PLATFORMS))"
	docker buildx build \
	  --platform $(PLATFORMS) \
	  --file docker/host.Dockerfile \
	  --tag $(REGISTRY)/playgate-host:$(TAG) \
	  --tag $(REGISTRY)/playgate-host:latest \
	  --push \
	  .

docker-server:
	@echo "==> docker buildx server ($(PLATFORMS))"
	docker buildx build \
	  --platform $(PLATFORMS) \
	  --file docker/server.Dockerfile \
	  --tag $(REGISTRY)/playgate-server:$(TAG) \
	  --tag $(REGISTRY)/playgate-server:latest \
	  --push \
	  .

# ── Helpers ──────────────────────────────────────────────────────────────────
$(DIST):
	mkdir -p $(DIST)

clean:
	rm -rf $(DIST)
