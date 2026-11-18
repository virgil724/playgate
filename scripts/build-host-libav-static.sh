#!/usr/bin/env bash
# Build playgate-host with `-tags libav` fully statically against the prefix
# produced by scripts/build-ffmpeg-static.sh. Must run on Alpine (musl) with
# `deps` already applied; installs the Go toolchain version from go.mod if needed.
#
# Usage: scripts/build-host-libav-static.sh <PREFIX> <OUTDIR> [VERSION]
# Output: <OUTDIR>/playgate-host-libav-static-linux-<arch>
#         <OUTDIR>/playgate-host-libav-static-linux-<arch>.sha256
#         <OUTDIR>/playgate-host-libav-static-linux-<arch>.BUILDINFO.txt
set -euo pipefail

PREFIX="${1:?usage: $0 <PREFIX> <OUTDIR> [VERSION]}"
OUTDIR="${2:?usage: $0 <PREFIX> <OUTDIR> [VERSION]}"
VERSION="${3:-dev}"

[ -f "$PREFIX/.complete" ] || { echo "FFmpeg prefix $PREFIX not built" >&2; exit 1; }

case "$(uname -m)" in
x86_64) GOARCH=amd64 ;;
aarch64) GOARCH=arm64 ;;
*) echo "unsupported arch $(uname -m)" >&2; exit 1 ;;
esac

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HOST="$ROOT/playgate-host"
NAME="playgate-host-libav-static-linux-${GOARCH}"
mkdir -p "$OUTDIR"
OUTDIR="$(cd "$OUTDIR" && pwd)"

# --- Go toolchain (version from go.mod) -------------------------------------
if ! command -v go >/dev/null 2>&1; then
  gv="$(awk '/^go /{print $2; exit}' "$HOST/go.mod")"
  case "$gv" in *.*.*) ;; *) gv="${gv}.0" ;; esac
  apk add --no-cache libc6-compat >/dev/null
  curl -fsSL "https://go.dev/dl/go${gv}.linux-${GOARCH}.tar.gz" | tar -C /usr/local -xz
  export PATH="/usr/local/go/bin:$PATH"
fi
go version

# --- link against static FFmpeg ---------------------------------------------
export PKG_CONFIG_PATH="$PREFIX/lib/pkgconfig"
LIBS="libavdevice libavfilter libavformat libavcodec libswresample libswscale libavutil"
# shellcheck disable=SC2086
CGO_CFLAGS="$(pkg-config --cflags $LIBS)"
# shellcheck disable=SC2086
CGO_LDFLAGS="$(pkg-config --static --libs $LIBS)"
export CGO_CFLAGS CGO_LDFLAGS
export CGO_ENABLED=1 GOOS=linux GOARCH="$GOARCH"

TAGS="libav,netgo,osusergo"
EXT='-linkmode external -extldflags "-static"'
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
LDFLAGS="-s -w -X main.Version=${VERSION} -X main.Commit=${GITHUB_SHA:-unknown} -X main.BuildDate=${BUILD_DATE} ${EXT}"

cd "$HOST"
echo "==> go test -tags $TAGS ./internal/encoder/libav/..."
go test -tags "$TAGS" -count=1 -ldflags "$EXT" ./internal/encoder/libav/...

echo "==> go build"
go build -buildvcs=false -tags "$TAGS" -trimpath -ldflags "$LDFLAGS" -o "$OUTDIR/$NAME" ./cmd/host

# --- verify static -----------------------------------------------------------
BIN="$OUTDIR/$NAME"
file "$BIN"
if ! file "$BIN" | grep -Eq 'statically linked|static-pie linked'; then
  echo "FAIL: $BIN is not statically linked" >&2
  exit 1
fi
if ldd "$BIN" >/dev/null 2>&1; then
  echo "FAIL: ldd reports dynamic dependencies" >&2
  ldd "$BIN" >&2
  exit 1
fi
echo "ldd: not a dynamic executable (OK)"

# --- smoke test (-h exits 0 via flag.ErrHelp) --------------------------------
"$BIN" -h 2>&1 | head -n 5 || true

( cd "$OUTDIR" && sha256sum "$NAME" >"$NAME.sha256" )
{
  cat "$PREFIX/BUILDINFO.txt"
  echo
  echo "playgate-host version: ${VERSION}"
  echo "go: $(go version)"
  echo "go-astiav: $(go list -m -f '{{.Path}} {{.Version}}' github.com/asticode/go-astiav 2>/dev/null || echo unknown)"
  echo "go build tags: ${TAGS}"
  echo "go ldflags (link): ${EXT}"
  echo "CGO_LDFLAGS: ${CGO_LDFLAGS}"
} >"$OUTDIR/$NAME.BUILDINFO.txt"

if [ -n "${HOST_UID:-}" ]; then chown -R "${HOST_UID}:${HOST_GID:-$HOST_UID}" "$OUTDIR" || true; fi
echo "==> built $BIN"
