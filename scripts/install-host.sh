#!/usr/bin/env bash
# scripts/install-host.sh — PlayGate Host one-click installer
# Supports: Raspberry Pi OS (Debian Bookworm/Bullseye), Ubuntu 22.04+, Debian 11+
# ─────────────────────────────────────────────────────────────────────────────
# Usage:
#   curl -fsSL https://your.cdn/install-host.sh | sudo bash
#   sudo bash install-host.sh                      # install
#   sudo bash install-host.sh --uninstall          # remove everything
#   sudo bash install-host.sh --version 1.2.3      # install specific version
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

# ── Constants ─────────────────────────────────────────────────────────────────
INSTALL_DIR="/opt/playgate"
BIN_DIR="/usr/local/bin"
CONFIG_DIR="/etc/playgate"
SYSTEMD_DIR="/etc/systemd/system"
PLAYGATE_USER="playgate"
NXBT_SOCKET_DIR="/run/nxbt"
GITHUB_REPO="playgate/playgate"         # adjust to real org/repo
DEFAULT_VERSION="latest"

# BlueZ config paths
BLUEZ_SERVICE_SRC="/lib/systemd/system/bluetooth.service"
BLUEZ_SERVICE_OVERRIDE="/etc/systemd/system/bluetooth.service.d/playgate-noplugin.conf"
BLUEZ_MAIN_CONF="/etc/bluetooth/main.conf"
BLUEZ_MAIN_CONF_BACKUP="/etc/bluetooth/main.conf.playgate-bak"

# Colours
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
info()    { echo -e "${GREEN}[playgate]${NC} $*"; }
warn()    { echo -e "${YELLOW}[playgate] WARN:${NC} $*"; }
error()   { echo -e "${RED}[playgate] ERROR:${NC} $*" >&2; }
die()     { error "$*"; exit 1; }

# ── Argument parsing ─────────────────────────────────────────────────────────
UNINSTALL=false
VERSION="${DEFAULT_VERSION}"
ARCH=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --uninstall)  UNINSTALL=true ;;
        --version)    VERSION="$2"; shift ;;
        --arch)       ARCH="$2"; shift ;;
        -h|--help)
            echo "Usage: $0 [--uninstall] [--version VERSION] [--arch ARCH]"
            exit 0
            ;;
        *) die "Unknown option: $1" ;;
    esac
    shift
done

# ── Root check ───────────────────────────────────────────────────────────────
[[ $EUID -eq 0 ]] || die "This script must be run as root (sudo $0)"

# ── Detect architecture ──────────────────────────────────────────────────────
detect_arch() {
    local machine
    machine="$(uname -m)"
    case "$machine" in
        aarch64|arm64) echo "arm64" ;;
        x86_64|amd64)  echo "amd64" ;;
        *) die "Unsupported architecture: $machine" ;;
    esac
}
[[ -z "$ARCH" ]] && ARCH="$(detect_arch)"

# ── Uninstall ─────────────────────────────────────────────────────────────────
do_uninstall() {
    info "Uninstalling PlayGate Host..."

    for svc in playgate-host nxbtd; do
        if systemctl is-enabled --quiet "$svc" 2>/dev/null; then
            systemctl disable --now "$svc" || true
        fi
        rm -f "${SYSTEMD_DIR}/${svc}.service"
    done

    systemctl daemon-reload

    rm -f "${BIN_DIR}/playgate-host"
    rm -rf "${INSTALL_DIR}"
    # Leave /etc/playgate and config.yaml so the user doesn't lose their config
    warn "Config at ${CONFIG_DIR} was NOT removed. Remove manually if desired."

    # Revert BlueZ changes
    if [[ -f "${BLUEZ_MAIN_CONF_BACKUP}" ]]; then
        info "Restoring ${BLUEZ_MAIN_CONF} from backup..."
        cp "${BLUEZ_MAIN_CONF_BACKUP}" "${BLUEZ_MAIN_CONF}"
        rm -f "${BLUEZ_MAIN_CONF_BACKUP}"
    fi
    rm -f "${BLUEZ_SERVICE_OVERRIDE}"
    systemctl daemon-reload
    systemctl restart bluetooth || true

    # Remove user (but not home dir, in case of existing data)
    if id "${PLAYGATE_USER}" &>/dev/null; then
        userdel "${PLAYGATE_USER}" || true
    fi

    info "Uninstall complete."
}

if $UNINSTALL; then
    do_uninstall
    exit 0
fi

# ══════════════════════════════════════════════════════════════════════════════
# INSTALL
# ══════════════════════════════════════════════════════════════════════════════

info "PlayGate Host installer — arch=${ARCH}, version=${VERSION}"

# ── 1. System dependencies ───────────────────────────────────────────────────
info "Installing system packages..."
apt-get update -qq
apt-get install -y --no-install-recommends \
    curl \
    ffmpeg \
    bluez \
    bluetooth \
    git \
    python3 \
    python3-pip \
    python3-dbus \
    python3-gi \
    dbus \
    v4l-utils \
    ca-certificates

# ── 2. Install nuxbt Python library ─────────────────────────────────────────
# nuxbt is an actively maintained fork of NXBT, pinned to a known-good tag.
info "Installing nuxbt Python library..."
NUXBT_PKG="git+https://github.com/hannahbee91/nuxbt.git@v3.3.6"
# Use --break-system-packages on Debian Bookworm+ (PEP 668)
pip3 install --no-cache-dir --break-system-packages "${NUXBT_PKG}" 2>/dev/null \
    || pip3 install --no-cache-dir "${NUXBT_PKG}"

# ── 3. BlueZ configuration ───────────────────────────────────────────────────
# nuxbt requires two BlueZ changes:
#   a) Disable the "input" plugin (it claims HID profiles, blocking nuxbt)
#   b) Enable experimental features (required by some BlueZ versions)
#
# We use a systemd drop-in for (a) — safer than editing the unit file.
# We patch /etc/bluetooth/main.conf for (b) — with backup.
info "Configuring BlueZ for nuxbt..."

## 3a — Disable BlueZ input plugin via systemd drop-in override
info "  Creating systemd drop-in: ${BLUEZ_SERVICE_OVERRIDE}"
mkdir -p "$(dirname "${BLUEZ_SERVICE_OVERRIDE}")"
# Read ExecStart from the system unit and append --noplugin=input
ORIG_EXECSTART=""
if [[ -f "${BLUEZ_SERVICE_SRC}" ]]; then
    ORIG_EXECSTART=$(grep -E '^ExecStart=' "${BLUEZ_SERVICE_SRC}" | head -1 || true)
fi
if [[ -z "${ORIG_EXECSTART}" ]]; then
    # Fallback to known default on Debian/RPi
    ORIG_EXECSTART="ExecStart=/usr/lib/bluetooth/bluetoothd"
fi

# Strip any trailing flags (idempotent) then append --noplugin=input
EXECSTART_BASE=$(echo "${ORIG_EXECSTART}" | sed 's/ --noplugin=input//')
EXECSTART_NEW="${EXECSTART_BASE} --noplugin=input"

cat > "${BLUEZ_SERVICE_OVERRIDE}" <<EOF
# PlayGate auto-generated override — disables BlueZ input plugin so nuxbt
# can register a Nintendo Switch Pro Controller HID profile.
# Generated by: install-host.sh $(date -u +%Y-%m-%dT%H:%M:%SZ)
[Service]
ExecStart=
${EXECSTART_NEW}
EOF
info "  BlueZ override written: --noplugin=input appended to ExecStart"

## 3b — Enable experimental features in /etc/bluetooth/main.conf
info "  Patching ${BLUEZ_MAIN_CONF} (experimental features + AutoEnable)..."

if [[ ! -f "${BLUEZ_MAIN_CONF}" ]]; then
    # Create minimal config if it doesn't exist
    mkdir -p "$(dirname "${BLUEZ_MAIN_CONF}")"
    touch "${BLUEZ_MAIN_CONF}"
fi

# Back up original (only once — don't overwrite existing backup)
if [[ ! -f "${BLUEZ_MAIN_CONF_BACKUP}" ]]; then
    cp "${BLUEZ_MAIN_CONF}" "${BLUEZ_MAIN_CONF_BACKUP}"
    info "  Backup saved: ${BLUEZ_MAIN_CONF_BACKUP}"
else
    info "  Backup already exists, skipping: ${BLUEZ_MAIN_CONF_BACKUP}"
fi

# Patch [General] section: set Experimental = true
# Idempotent: sed replaces existing value or appends under [General]
if grep -q '^\[General\]' "${BLUEZ_MAIN_CONF}"; then
    # Replace or insert Experimental under [General]
    if grep -q '^Experimental' "${BLUEZ_MAIN_CONF}"; then
        sed -i 's/^Experimental\s*=.*/Experimental = true/' "${BLUEZ_MAIN_CONF}"
    else
        sed -i '/^\[General\]/a Experimental = true' "${BLUEZ_MAIN_CONF}"
    fi
else
    # No [General] section — append it
    printf '\n[General]\nExperimental = true\n' >> "${BLUEZ_MAIN_CONF}"
fi

# Patch [Policy] section: set AutoEnable = true
if grep -q '^\[Policy\]' "${BLUEZ_MAIN_CONF}"; then
    if grep -q '^AutoEnable' "${BLUEZ_MAIN_CONF}"; then
        sed -i 's/^AutoEnable\s*=.*/AutoEnable = true/' "${BLUEZ_MAIN_CONF}"
    else
        sed -i '/^\[Policy\]/a AutoEnable = true' "${BLUEZ_MAIN_CONF}"
    fi
else
    printf '\n[Policy]\nAutoEnable = true\n' >> "${BLUEZ_MAIN_CONF}"
fi

info "  BlueZ main.conf patched (Experimental=true, AutoEnable=true)"

# Reload and restart bluetooth
systemctl daemon-reload
systemctl enable bluetooth
systemctl restart bluetooth
info "  bluetoothd restarted with --noplugin=input"

# ── 4. Create playgate system user ───────────────────────────────────────────
info "Creating system user: ${PLAYGATE_USER}"
if ! id "${PLAYGATE_USER}" &>/dev/null; then
    useradd \
        --system \
        --user-group \
        --no-create-home \
        --shell /usr/sbin/nologin \
        --comment "PlayGate Host service account" \
        "${PLAYGATE_USER}"
fi

# Add to required groups
# video: access /dev/video* (V4L2 capture card)
# bluetooth: access bluetoothd socket (for future direct use)
for grp in video bluetooth; do
    if getent group "$grp" &>/dev/null; then
        usermod -aG "$grp" "${PLAYGATE_USER}"
        info "  Added ${PLAYGATE_USER} to group: $grp"
    else
        warn "  Group '$grp' not found — skipping"
    fi
done

# ── 5. Download binary ───────────────────────────────────────────────────────
info "Downloading playgate-host binary (${VERSION}, ${ARCH})..."
mkdir -p "${BIN_DIR}"

if [[ "${VERSION}" == "latest" ]]; then
    DOWNLOAD_URL="https://github.com/${GITHUB_REPO}/releases/latest/download/playgate-host-linux-${ARCH}"
else
    DOWNLOAD_URL="https://github.com/${GITHUB_REPO}/releases/download/v${VERSION}/playgate-host-linux-${ARCH}"
fi

TMPBIN="$(mktemp)"
trap 'rm -f "${TMPBIN}"' EXIT

if command -v curl &>/dev/null; then
    curl -fsSL --progress-bar "${DOWNLOAD_URL}" -o "${TMPBIN}"
elif command -v wget &>/dev/null; then
    wget -q --show-progress "${DOWNLOAD_URL}" -O "${TMPBIN}"
else
    die "Neither curl nor wget found — cannot download binary"
fi

chmod +x "${TMPBIN}"
mv "${TMPBIN}" "${BIN_DIR}/playgate-host"
info "  Binary installed: ${BIN_DIR}/playgate-host"

# ── 6. Install nxbt-daemon Python files ─────────────────────────────────────
info "Installing nxbt-daemon..."
mkdir -p "${INSTALL_DIR}/nxbt-daemon"

# In a real release, nxbt-daemon would be bundled in the release tarball.
# For now we check if we're running from the repo or download a placeholder.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NXBT_SRC="${SCRIPT_DIR}/../playgate-host/nxbt-daemon"

if [[ -d "${NXBT_SRC}" ]]; then
    cp "${NXBT_SRC}/nxbtd.py"           "${INSTALL_DIR}/nxbt-daemon/"
    cp "${NXBT_SRC}/requirements.txt"   "${INSTALL_DIR}/nxbt-daemon/"
    info "  nxbt-daemon copied from repo"
else
    warn "  nxbt-daemon source not found at ${NXBT_SRC}"
    warn "  Download nxbtd.py from the PlayGate release and place it at"
    warn "  ${INSTALL_DIR}/nxbt-daemon/nxbtd.py"
fi

chmod +x "${INSTALL_DIR}/nxbt-daemon/nxbtd.py" 2>/dev/null || true

# ── 7. Generate config.yaml template ─────────────────────────────────────────
mkdir -p "${CONFIG_DIR}"

if [[ -f "${CONFIG_DIR}/config.yaml" ]]; then
    info "config.yaml already exists — not overwriting"
else
    info "Generating config template: ${CONFIG_DIR}/config.yaml"
    cat > "${CONFIG_DIR}/config.yaml" <<'YAML'
# PlayGate Host configuration — generated by install-host.sh
# Edit this file, then restart: systemctl restart playgate-host

capture:
  source: v4l2
  device: /dev/video0   # change if your capture card is video1, video2, etc.
  width: 1280
  height: 720
  fps: 30
  format: YUYV

encoder:
  bitrate: 6000000
  preset: ultrafast
  keyframe_interval: 60
  ffmpeg_path: ffmpeg

webrtc:
  ice_servers:
    - stun:stun.l.google.com:19302

input:
  target: nxbt
  socket_path: /run/nxbt/nxbt.sock
  rate_hz: 120

session:
  enabled: false
  public_key_base64: ""
  public_key_file: ""
  idle_timeout_seconds: 60

signaling:
  url: https://REPLACE_WITH_YOUR_WORKER.workers.dev
  room_id: default
  token: ""
  poll_interval_ms: 500
  use_turn: false

metrics:
  report_interval_seconds: 5
YAML
    chown root:"${PLAYGATE_USER}" "${CONFIG_DIR}/config.yaml"
    chmod 640 "${CONFIG_DIR}/config.yaml"
    warn "IMPORTANT: Edit ${CONFIG_DIR}/config.yaml (set signaling.url!) before starting the service"
fi

# ── 8. Install systemd units ─────────────────────────────────────────────────
info "Installing systemd units..."

SCRIPT_DIR_ABS="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SYSTEMD_SRC="${SCRIPT_DIR_ABS}/systemd"

for unit in nxbtd.service playgate-host.service; do
    if [[ -f "${SYSTEMD_SRC}/${unit}" ]]; then
        cp "${SYSTEMD_SRC}/${unit}" "${SYSTEMD_DIR}/${unit}"
        info "  Installed ${SYSTEMD_DIR}/${unit}"
    else
        warn "  Unit file not found at ${SYSTEMD_SRC}/${unit} — writing fallback"
    fi
done

# Write fallback units inline if source files are missing (e.g. curl-piped install)
if [[ ! -f "${SYSTEMD_DIR}/nxbtd.service" ]]; then
    cat > "${SYSTEMD_DIR}/nxbtd.service" <<EOF
[Unit]
Description=PlayGate NXBT Bluetooth daemon
After=bluetooth.service
Requires=bluetooth.service

[Service]
Type=simple
ExecStart=/usr/bin/python3 ${INSTALL_DIR}/nxbt-daemon/nxbtd.py
WorkingDirectory=${INSTALL_DIR}/nxbt-daemon
Restart=on-failure
RestartSec=5
RuntimeDirectory=nxbt
RuntimeDirectoryMode=0755
User=root

[Install]
WantedBy=multi-user.target
EOF
fi

if [[ ! -f "${SYSTEMD_DIR}/playgate-host.service" ]]; then
    cat > "${SYSTEMD_DIR}/playgate-host.service" <<EOF
[Unit]
Description=PlayGate Host
After=network-online.target nxbtd.service
Wants=network-online.target
Requires=nxbtd.service

[Service]
Type=simple
ExecStart=${BIN_DIR}/playgate-host -config ${CONFIG_DIR}/config.yaml
User=${PLAYGATE_USER}
Group=${PLAYGATE_USER}
Restart=on-failure
RestartSec=5
# Allow access to /run/nxbt socket
SupplementaryGroups=video bluetooth
# Security hardening
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ReadWritePaths=/var/log

[Install]
WantedBy=multi-user.target
EOF
fi

# ── 9. Enable and start services ─────────────────────────────────────────────
systemctl daemon-reload
systemctl enable nxbtd
systemctl enable playgate-host
systemctl start nxbtd

info "Waiting for nxbt socket..."
WAIT=0
until [[ -S "${NXBT_SOCKET_DIR}/nxbt.sock" ]] || [[ $WAIT -ge 10 ]]; do
    sleep 1
    WAIT=$((WAIT + 1))
done

systemctl start playgate-host

# ── 10. Summary ──────────────────────────────────────────────────────────────
echo ""
echo "═══════════════════════════════════════════════════════"
info "PlayGate Host installation complete!"
echo "═══════════════════════════════════════════════════════"
echo ""
echo "  Config:     ${CONFIG_DIR}/config.yaml"
echo "  Binary:     ${BIN_DIR}/playgate-host"
echo "  nxbt:       ${INSTALL_DIR}/nxbt-daemon/nxbtd.py"
echo ""
echo "  Services:   systemctl status playgate-host"
echo "              systemctl status nxbtd"
echo ""
echo "  Logs:       journalctl -u playgate-host -f"
echo "              journalctl -u nxbtd -f"
echo ""
echo "  IMPORTANT:  Edit ${CONFIG_DIR}/config.yaml"
echo "              Set signaling.url to your Cloudflare Worker URL"
echo "              Then: systemctl restart playgate-host"
echo ""
if [[ -f "${CONFIG_DIR}/config.yaml" ]] && \
   grep -q 'REPLACE_WITH_YOUR_WORKER' "${CONFIG_DIR}/config.yaml"; then
    warn "  signaling.url is still a placeholder — service will not connect until set!"
fi
