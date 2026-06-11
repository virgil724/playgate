# nxbt-daemon

Python daemon that bridges the Go host process to a Nintendo Switch Pro
Controller emulated via the [nuxbt](https://github.com/hannahbee91/nuxbt)
library over Bluetooth. nuxbt is an actively maintained fork of NXBT; Switch 2
controller emulation is on its roadmap (not yet implemented). PlayGate pins
the `v3.3.6` tag.

The daemon, socket path and unit names keep the historical `nxbt` prefix —
only the underlying Python library changed.

Communication happens over a Unix domain socket using newline-delimited JSON.
See `docs/protocols.md` (repo root of playgate-host) for the full wire-format
specification.

---

## Platform requirements

- **Linux only** (nuxbt requires BlueZ and DBus).
- Python 3.11+
- BlueZ >= 5.50
- Root or CAP_NET_ADMIN capability (nuxbt uses DBus to talk to bluetoothd)
- `python3-dbus` and `python3-gi` apt packages (or pip-built equivalents)

---

## BlueZ prerequisites

### 1. Disable the BlueZ input plugin

The stock BlueZ `input` plugin claims HID profiles and prevents nuxbt from
registering the controller. Disable it:

```
sudo nano /lib/systemd/system/bluetooth.service
```

Change the `ExecStart` line to append `--noplugin=input`:

```
ExecStart=/usr/lib/bluetooth/bluetoothd --noplugin=input
```

Then reload:

```
sudo systemctl daemon-reload
sudo systemctl restart bluetooth
```

### 2. Enable experimental features (BlueZ 5.53+)

Some BlueZ versions require `Experimental = true` in `/etc/bluetooth/main.conf`:

```ini
[Policy]
AutoEnable=true

[General]
Experimental = true
```

### 3. Verify bluetoothd is accessible

```
sudo systemctl status bluetooth
bluetoothctl show    # should list your adapter
```

---

## Installation

```bash
# Clone playgate-host, then:
cd playgate-host/nxbt-daemon

# System prerequisites (provide the dbus/gi Python bindings without compiling)
sudo apt install git python3-dbus python3-gi

# Option A — system-wide (requires root for Bluetooth)
sudo pip3 install --break-system-packages -r requirements.txt

# Option B — virtualenv (--system-site-packages exposes apt's dbus/gi modules)
python3 -m venv --system-site-packages venv
source venv/bin/activate
pip install -r requirements.txt
```

---

## Running manually

```bash
# Real mode — forwards inputs to a paired Switch over Bluetooth
sudo python3 nxbtd.py

# Specify a custom socket path
sudo python3 nxbtd.py --socket /tmp/nxbt.sock

# Mock mode — logs inputs to stdout, no Bluetooth required
python3 nxbtd.py --mock

# Debug logging
sudo python3 nxbtd.py --debug
```

The daemon starts listening on `/run/nxbt/nxbt.sock` by default. The Go host
process connects to this socket as a client.

---

## systemd unit example

Save as `/etc/systemd/system/nxbt-daemon.service`:

```ini
[Unit]
Description=PlayGate NXBT daemon
After=bluetooth.service
Requires=bluetooth.service

[Service]
Type=simple
ExecStart=/usr/bin/python3 /opt/playgate/nxbt-daemon/nxbtd.py
WorkingDirectory=/opt/playgate/nxbt-daemon
Restart=on-failure
RestartSec=5
RuntimeDirectory=nxbt
RuntimeDirectoryMode=0755
# Root required for nuxbt / Bluetooth access
User=root

[Install]
WantedBy=multi-user.target
```

Enable and start:

```bash
sudo systemctl daemon-reload
sudo systemctl enable nxbt-daemon
sudo systemctl start nxbt-daemon
journalctl -u nxbt-daemon -f
```

---

## Mock mode

If `nuxbt` cannot be imported (wrong platform, missing BlueZ, or intentional
`--mock` flag), the daemon falls back to **mock mode** automatically. In mock
mode:

- No Bluetooth hardware is required.
- All controller inputs are logged to stdout/stderr instead of being forwarded
  to a real Switch.
- The Unix socket is still opened and the JSON protocol is fully honoured, so
  the Go host can be tested without a physical Switch.

To force mock mode regardless of whether nuxbt is installed:

```bash
python3 nxbtd.py --mock
```

---

## Protocol summary

See `docs/protocols.md` for the authoritative byte-level specification.
Quick reference:

**Go → daemon** (newline-delimited JSON on the Unix socket):

```json
{"type":"input","buttons":1,"lx":0.5,"ly":-0.5,"rx":0.0,"ry":0.0}
{"type":"ping"}
```

**daemon → Go**:

```json
{"type":"status","state":"connected","detail":"Switch paired to 01:23:45:67:89:AB"}
{"type":"pong"}
```

---

## Running tests

The test suite (`test_protocol.py`) does **not** import `nuxbt` and runs on any
platform (Linux, macOS, Windows):

```bash
cd nxbt-daemon
python3 -m pytest test_protocol.py -v
# or without pytest:
python3 test_protocol.py
```
