#!/usr/bin/env bash
# Build x264 + FFmpeg as STATIC libraries into a prefix, for the fully
# self-contained `-tags libav` playgate-host build (musl / Alpine).
#
# Usage:
#   scripts/build-ffmpeg-static.sh deps                 # apk add build deps (Alpine)
#   scripts/build-ffmpeg-static.sh build <PREFIX>       # build + install into PREFIX
#
# Env overrides: FFMPEG_VERSION, X264_REF, JOBS, HOST_UID/HOST_GID (chown result).
# Also writes <PREFIX>/BUILDINFO.txt (versions + configure lines + source sha256)
# which is shipped next to the binary as the GPL source-offer record.
#
# Local use (any host with docker):
#   docker run --rm -v "$PWD":/src -w /src alpine:3.20 sh -c \
#     'apk add --no-cache bash && scripts/build-ffmpeg-static.sh deps && \
#      scripts/build-ffmpeg-static.sh build /opt/ffstatic'
set -euo pipefail

FFMPEG_VERSION="${FFMPEG_VERSION:-6.1.1}"
# x264 stable branch commit (x264 0.164). Override to bump.
X264_REF="${X264_REF:-31e19f92f00c7003fa115047ce50978bc98c3a0d}"
JOBS="${JOBS:-$(nproc)}"

FFMPEG_URL="https://ffmpeg.org/releases/ffmpeg-${FFMPEG_VERSION}.tar.xz"
X264_URL="https://code.videolan.org/videolan/x264/-/archive/${X264_REF}/x264-${X264_REF}.tar.gz"

cmd="${1:-}"

case "$cmd" in
deps)
  apk add --no-cache build-base nasm yasm pkgconf curl xz tar git linux-headers \
    perl bash coreutils file
  exit 0
  ;;
build) ;;
*)
  echo "usage: $0 deps | build <PREFIX>" >&2
  exit 2
  ;;
esac

PREFIX="${2:?usage: $0 build <PREFIX>}"
mkdir -p "$PREFIX"

# --- configure lines (single source of truth; the workflow cache key hashes
# this script, and the lines are recorded in BUILDINFO.txt) --------------------
X264_CONFIGURE=(
  --prefix="$PREFIX" --libdir="$PREFIX/lib"
  --enable-static --disable-shared --enable-pic
  --disable-cli --disable-opencl --disable-avs --disable-swscale
  --disable-lavf --disable-ffms --disable-gpac --disable-lsmash
  --bit-depth=8
)

# Minimal FFmpeg: keep all 7 libs go-astiav links (avcodec avdevice avfilter
# avformat avutil swresample swscale) but strip components. NVENC is deliberately
# absent (see docs/deploy.md: a static musl binary cannot dlopen the NVIDIA
# driver). VAAPI is skipped (needs dynamic libva).
FFMPEG_CONFIGURE=(
  --prefix="$PREFIX" --libdir="$PREFIX/lib"
  --pkg-config-flags=--static
  --extra-cflags="-I$PREFIX/include" --extra-ldflags="-L$PREFIX/lib"
  --enable-gpl --enable-libx264
  --enable-static --disable-shared --enable-pic
  --disable-programs --disable-doc --disable-debug
  --disable-autodetect --disable-network
  --disable-encoders --enable-encoder=libx264,h264_v4l2m2m,rawvideo
  --disable-decoders --enable-decoder=rawvideo,mjpeg,h264
  --disable-hwaccels
  --disable-muxers --enable-muxer=h264,rawvideo,null
  --disable-demuxers --enable-demuxer=h264,rawvideo,mjpeg
  --disable-parsers --enable-parser=h264,mjpeg
  --disable-protocols --enable-protocol=file,pipe
  --disable-indevs --enable-indev=v4l2,lavfi
  --disable-outdevs
  --disable-filters
  --enable-filter=buffer,buffersink,scale,format,fps,null,crop,pad,hflip,vflip
  --enable-v4l2-m2m
)

mkdir -p "$PREFIX/.src"
cd "$PREFIX/.src"

fetch() { # url outfile
  [ -f "$2" ] || curl -fsSL --retry 3 -o "$2" "$1"
}

fetch "$X264_URL" x264.tar.gz
fetch "$FFMPEG_URL" ffmpeg.tar.xz
X264_SHA256="$(sha256sum x264.tar.gz | cut -d' ' -f1)"
FFMPEG_SHA256="$(sha256sum ffmpeg.tar.xz | cut -d' ' -f1)"

rm -rf x264-src ffmpeg-src
mkdir x264-src ffmpeg-src
tar -xzf x264.tar.gz -C x264-src --strip-components=1
tar -xJf ffmpeg.tar.xz -C ffmpeg-src --strip-components=1

echo "==> x264 ${X264_REF}"
(
  cd x264-src
  ./configure "${X264_CONFIGURE[@]}"
  make -j"$JOBS"
  make install
)

echo "==> FFmpeg ${FFMPEG_VERSION}"
(
  cd ffmpeg-src
  PKG_CONFIG_PATH="$PREFIX/lib/pkgconfig" ./configure "${FFMPEG_CONFIGURE[@]}"
  make -j"$JOBS"
  make install
)

{
  echo "playgate-host libav static build - component record (GPL source offer)"
  echo "arch:            $(uname -m)"
  echo "libc:            musl (Alpine)"
  echo "FFmpeg:          ${FFMPEG_VERSION}"
  echo "FFmpeg source:   ${FFMPEG_URL}"
  echo "FFmpeg sha256:   ${FFMPEG_SHA256}"
  echo "x264 commit:     ${X264_REF}"
  echo "x264 source:     ${X264_URL}"
  echo "x264 sha256:     ${X264_SHA256}"
  echo
  echo "x264 configure:    ${X264_CONFIGURE[*]}"
  echo
  echo "FFmpeg configure:  ${FFMPEG_CONFIGURE[*]}"
  echo
  echo "License: FFmpeg built with --enable-gpl + libx264 => this binary is GPL (v2 or later)."
  echo "Corresponding source: the URLs above (unmodified upstream) plus the playgate-host"
  echo "source at the tagged release; build scripts: scripts/build-ffmpeg-static.sh,"
  echo "scripts/build-host-libav-static.sh."
} >"$PREFIX/BUILDINFO.txt"

# Source trees are not needed afterwards; keep the cache small.
cd "$PREFIX"
rm -rf .src
touch "$PREFIX/.complete"

if [ -n "${HOST_UID:-}" ]; then chown -R "${HOST_UID}:${HOST_GID:-$HOST_UID}" "$PREFIX" || true; fi
echo "==> done: $PREFIX"
