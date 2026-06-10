#!/bin/sh
# docker/host-entrypoint.sh — container entrypoint for playgate-host
# Starts the nxbt daemon in the background, waits for its socket,
# then exec's playgate-host so it receives signals directly.
set -eu

CONFIG="${PLAYGATE_CONFIG:-/etc/playgate/config.yaml}"
SOCKET="${NXBT_SOCKET:-/run/nxbt/nxbt.sock}"
NXBT_DAEMON_DIR="/opt/playgate/nxbt-daemon"

if [ ! -f "$CONFIG" ]; then
    echo "[entrypoint] WARNING: config not found at $CONFIG"
    echo "[entrypoint] Copy config.example.yaml → $CONFIG and re-run"
    echo "[entrypoint] Mount example: -v /host/path/config.yaml:$CONFIG:ro"
    exit 1
fi

# Start nxbt daemon (needs --privileged / CAP_NET_ADMIN + dbus access)
echo "[entrypoint] Starting nxbt daemon..."
python3 "${NXBT_DAEMON_DIR}/nxbtd.py" --socket "${SOCKET}" &
NXBT_PID=$!

# Wait up to 10 s for the socket to appear
WAIT=0
until [ -S "$SOCKET" ] || [ $WAIT -ge 10 ]; do
    sleep 1
    WAIT=$((WAIT + 1))
    echo "[entrypoint] Waiting for nxbt socket... ($WAIT/10)"
done

if [ ! -S "$SOCKET" ]; then
    echo "[entrypoint] WARNING: nxbt socket not found after 10 s — host will retry"
fi

echo "[entrypoint] Starting playgate-host..."
exec /usr/local/bin/playgate-host -config "$CONFIG" "$@"
