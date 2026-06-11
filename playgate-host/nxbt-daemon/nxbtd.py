#!/usr/bin/env python3
"""
nxbtd.py — PlayGate NXBT daemon

Manages an emulated Switch Pro Controller over Bluetooth (via the nuxbt
library, an actively maintained fork of NXBT) and exposes it to the Go host
process via a Unix socket using newline-delimited JSON.

Protocol (Go ↔ daemon):
  Go → daemon:
    {"type":"input","buttons":<uint32>,"lx":..,"ly":..,"rx":..,"ry":..}
    {"type":"ping"}

  daemon → Go:
    {"type":"status","state":"connecting|connected|disconnected","detail":..}
    {"type":"pong"}

Run:
  python3 nxbtd.py [--socket /run/nxbt/nxbt.sock]

In mock mode (nuxbt unavailable):
  python3 nxbtd.py --mock
  or when nuxbt cannot be imported the daemon falls back to mock automatically.
"""

import argparse
import json
import logging
import multiprocessing
import multiprocessing.util
import os
import shutil
import signal
import socket
import subprocess
import sys
import threading
import time
from typing import Optional

# ---------------------------------------------------------------------------
# Button bitmask — MUST stay in sync with internal/core/types.go
# ---------------------------------------------------------------------------
#
# bit  0 (0x000001)  ButtonA         → "A"
# bit  1 (0x000002)  ButtonB         → "B"
# bit  2 (0x000004)  ButtonX         → "X"
# bit  3 (0x000008)  ButtonY         → "Y"
# bit  4 (0x000010)  ButtonL         → "L"
# bit  5 (0x000020)  ButtonR         → "R"
# bit  6 (0x000040)  ButtonZL        → "ZL"
# bit  7 (0x000080)  ButtonZR        → "ZR"
# bit  8 (0x000100)  ButtonPlus      → "PLUS"
# bit  9 (0x000200)  ButtonMinus     → "MINUS"
# bit 10 (0x000400)  ButtonHome      → "HOME"
# bit 11 (0x000800)  ButtonCapture   → "CAPTURE"
# bit 12 (0x001000)  ButtonLStick    → "L_STICK"
# bit 13 (0x002000)  ButtonRStick    → "R_STICK"
# bit 14 (0x004000)  ButtonDpadUp    → "DPAD_UP"
# bit 15 (0x008000)  ButtonDpadDown  → "DPAD_DOWN"
# bit 16 (0x010000)  ButtonDpadLeft  → "DPAD_LEFT"
# bit 17 (0x020000)  ButtonDpadRight → "DPAD_RIGHT"

BUTTON_MAP: list[tuple[int, str]] = [
    (0x000001, "A"),
    (0x000002, "B"),
    (0x000004, "X"),
    (0x000008, "Y"),
    (0x000010, "L"),
    (0x000020, "R"),
    (0x000040, "ZL"),
    (0x000080, "ZR"),
    (0x000100, "PLUS"),
    (0x000200, "MINUS"),
    (0x000400, "HOME"),
    (0x000800, "CAPTURE"),
    (0x001000, "L_STICK"),
    (0x002000, "R_STICK"),
    (0x004000, "DPAD_UP"),
    (0x008000, "DPAD_DOWN"),
    (0x010000, "DPAD_LEFT"),
    (0x020000, "DPAD_RIGHT"),
]

# ---------------------------------------------------------------------------
# Logging setup
#
# Must happen before the nuxbt import below: logging a warning first would
# implicitly configure the root logger at WARNING level and turn this
# basicConfig into a no-op, silently dropping every INFO log in mock mode.
# ---------------------------------------------------------------------------

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)-8s %(name)s %(message)s",
    datefmt="%Y-%m-%dT%H:%M:%S",
)
log = logging.getLogger("nxbtd")

# ---------------------------------------------------------------------------
# nuxbt import — fall back to mock mode gracefully
# ---------------------------------------------------------------------------

MOCK_MODE = False

try:
    import nuxbt  # type: ignore
    _nuxbt_available = True
except ImportError:
    _nuxbt_available = False
    MOCK_MODE = True
    log.warning(
        "nuxbt module not found — running in mock mode. "
        "Inputs will be logged but not forwarded to a real Switch."
    )

# ---------------------------------------------------------------------------
# BlueZ plugin gate (real mode only)
#
# Emulating a controller requires bluetoothd to run with --compat --noplugin=*
# (otherwise BlueZ's input plugin owns the HID L2CAP ports and pairing fails).
# nuxbt manages this via a systemd override at
# /run/systemd/system/bluetooth.service.d/nuxbt.conf — /run is tmpfs, so the
# override evaporates on every reboot and the daemon must self-heal at start.
# ---------------------------------------------------------------------------

# Detail string broadcast when the plugin is disabled and we cannot fix it.
PLUGIN_DISABLED_HINT = (
    "NUXBT BlueZ plugin not enabled — run 'sudo nuxbt toggle' "
    "(or run nxbtd as root) and the daemon will retry"
)

# How long to wait for bluetoothd to come back after `systemctl restart`.
BLUETOOTH_RESTART_WAIT = 2.0

# Controller supervision retry back-off (seconds).
RETRY_BACKOFF_INITIAL = 2.0
RETRY_BACKOFF_MAX = 60.0


class BluezPluginDisabledError(Exception):
    """The NUXBT BlueZ plugin override is missing and we lack root to fix it."""


def _import_bluez():
    """Lazily import nuxbt.bluez.

    Only ever called in real (non-mock) mode: nuxbt.bluez pulls in dbus and
    must never be imported in mock mode or CI. Module-level indirection so
    tests can inject a fake.
    """
    import nuxbt.bluez  # type: ignore
    return nuxbt.bluez


def _geteuid() -> int:
    """os.geteuid indirection so tests can fake the root / non-root paths."""
    return os.geteuid()


# ---------------------------------------------------------------------------
# Protocol helpers
# ---------------------------------------------------------------------------


def send_msg(sock: socket.socket, msg: dict) -> None:
    """Send a newline-terminated JSON message to sock."""
    line = json.dumps(msg, separators=(",", ":")) + "\n"
    sock.sendall(line.encode())


def buttons_to_nxbt(buttons: int) -> list[str]:
    """Convert a button bitmask to the list of nuxbt button name strings."""
    return [name for mask, name in BUTTON_MAP if buttons & mask]


def axis_to_nxbt(value: float) -> int:
    """Convert a normalised axis value [-1, 1] to nuxbt's integer range [-100, 100].

    nuxbt's input parser divides X_VALUE/Y_VALUE by 100 to obtain the stick
    ratio (see nuxbt/controller/input.py), so the wire range is -100..100.
    """
    clamped = max(-1.0, min(1.0, float(value)))
    return int(round(clamped * 100))


def fill_input_packet(
    pkt: dict,
    buttons: int,
    lx: float,
    ly: float,
    rx: float,
    ry: float,
) -> dict:
    """Fill a nuxbt input packet (from Nuxbt.create_input_packet()) in place.

    Packet layout (nuxbt DIRECT_INPUT_PACKET):
      - face/trigger/meta/dpad buttons are top-level booleans keyed by the
        same names our BUTTON_MAP uses ("A", "B", …, "DPAD_RIGHT"),
      - stick clicks live at pkt["L_STICK"]["PRESSED"] / pkt["R_STICK"]["PRESSED"],
      - stick axes live at pkt[stick]["X_VALUE"/"Y_VALUE"], integers -100..100.
    """
    for name in buttons_to_nxbt(buttons):
        if name == "L_STICK":
            pkt["L_STICK"]["PRESSED"] = True
        elif name == "R_STICK":
            pkt["R_STICK"]["PRESSED"] = True
        else:
            pkt[name] = True
    pkt["L_STICK"]["X_VALUE"] = axis_to_nxbt(lx)
    pkt["L_STICK"]["Y_VALUE"] = axis_to_nxbt(ly)
    pkt["R_STICK"]["X_VALUE"] = axis_to_nxbt(rx)
    pkt["R_STICK"]["Y_VALUE"] = axis_to_nxbt(ry)
    return pkt


# ---------------------------------------------------------------------------
# Controller backend (real NXBT or mock)
# ---------------------------------------------------------------------------


class MockController:
    """Simulates a controller without Bluetooth hardware."""

    def apply_input(
        self,
        buttons: int,
        lx: float,
        ly: float,
        rx: float,
        ry: float,
    ) -> None:
        pressed = buttons_to_nxbt(buttons)
        log.info(
            "[MOCK] buttons=%s lx=%.2f ly=%.2f rx=%.2f ry=%.2f",
            pressed,
            lx,
            ly,
            rx,
            ry,
        )

    def close(self) -> None:
        pass


# Auto-pairing sequence timings (seconds). Module-level so tests can zero them.
PAIR_PRESS_SECONDS = 0.2
PAIR_RELEASE_SECONDS = 0.5


class NxbtController:
    """Wraps the real nuxbt library."""

    def __init__(self) -> None:
        self._nx = nuxbt.Nuxbt()
        self._index: Optional[int] = None

    def _find_reconnect_targets(self) -> Optional[list]:
        """Return MACs of previously-paired Switches, or None for fresh pair.

        Mirrors the nuxbt TUI: reconnect targets come from BlueZ devices
        aliased "Nintendo Switch". Any lookup failure falls back to fresh
        pairing rather than aborting the connect attempt.
        """
        try:
            bluez = _import_bluez()
            addresses = bluez.find_devices_by_alias("Nintendo Switch")
        except Exception as exc:
            log.warning("could not query previously-paired Switches: %s", exc)
            return None
        return list(addresses) if addresses else None

    def connect(self, shutdown_event: threading.Event) -> None:
        """Create a Pro Controller and wait for the Switch to pair.

        Reconnects to a previously-paired Switch when one is known (the
        Switch can be anywhere, e.g. in a game); only does a fresh pair —
        which requires the Change Grip/Order menu — when none is known.
        """
        reconnect = self._find_reconnect_targets()
        if reconnect:
            log.info("previously-paired Switch(es) found: %s — reconnecting", reconnect)
            self._index = self._nx.create_controller(
                nuxbt.PRO_CONTROLLER, reconnect_address=reconnect
            )
        else:
            log.info("no previously-paired Switch — fresh pairing "
                     "(Switch must be on the Change Grip/Order menu)")
            self._index = self._nx.create_controller(nuxbt.PRO_CONTROLLER)
        log.info("nuxbt controller index=%d, waiting for Switch…", self._index)

        while not shutdown_event.is_set():
            state = self._nx.state[self._index]["state"]
            if state == "connected":
                break
            if state == "crashed":
                raise OSError(f"The controller has crashed with error: {self._nx.state[self._index].get('errors')}")
            time.sleep(0.1)

        if shutdown_event.is_set():
            log.info("Connect cancelled by shutdown event.")
            return

        if reconnect:
            # The Switch is NOT on the Grip/Order menu — pressing buttons
            # here would press them in whatever is currently running.
            log.info("Switch reconnected to controller index=%d — "
                     "skipping auto-pairing sequence", self._index)
            return

        log.info("Switch paired to controller index=%d, sending auto-pairing sequence...", self._index)

        # 1. Send L+R to register the controller
        pkt = self._nx.create_input_packet()
        pkt["L"] = True
        pkt["R"] = True
        self._nx.set_controller_input(self._index, pkt)
        if shutdown_event.wait(timeout=PAIR_PRESS_SECONDS):
            return

        # 2. Release L+R
        pkt = self._nx.create_input_packet()
        self._nx.set_controller_input(self._index, pkt)
        if shutdown_event.wait(timeout=PAIR_RELEASE_SECONDS):
            return

        # 3. Send A to confirm and exit pairing screen
        pkt = self._nx.create_input_packet()
        pkt["A"] = True
        self._nx.set_controller_input(self._index, pkt)
        if shutdown_event.wait(timeout=PAIR_PRESS_SECONDS):
            return

        # 4. Release A
        pkt = self._nx.create_input_packet()
        self._nx.set_controller_input(self._index, pkt)
        if shutdown_event.wait(timeout=PAIR_PRESS_SECONDS):
            return
        log.info("Auto-pairing sequence complete for controller index=%d", self._index)

    def apply_input(
        self,
        buttons: int,
        lx: float,
        ly: float,
        rx: float,
        ry: float,
    ) -> None:
        if self._index is None:
            return
        # A fresh packet must be obtained per input cycle.  create_input_packet()
        # returns a thread-safe copy of nuxbt's DIRECT_INPUT_PACKET template
        # (nuxbt deliberately avoids copy.deepcopy — do not deep-copy here).
        pkt = self._nx.create_input_packet()
        fill_input_packet(pkt, buttons, lx, ly, rx, ry)
        self._nx.set_controller_input(self._index, pkt)

    def close(self) -> None:
        if self._index is not None:
            try:
                self._nx.remove_controller(self._index)
            except Exception:
                pass
            self._index = None
        nx = getattr(self, "_nx", None)
        if nx is None:
            return
        # Terminate the controller manager process FIRST and gracefully.
        # Its own SIGTERM handler raises SystemExit, and its finally-block
        # (cm.shutdown()) terminates the controller session processes and
        # shuts down its inner Manager — grandchildren this process cannot
        # reach directly. A generous join: if it is killed before finishing,
        # those grandchildren leak (they are not our children to reap).
        controllers = getattr(nx, "controllers", None)
        if controllers is not None and controllers.is_alive():
            try:
                controllers.terminate()
                controllers.join(timeout=3.0)
            except Exception:
                pass
        # Terminate the BlueZ agent process (daemon=True, no children).
        agent = getattr(nx, "agent_process", None)
        if agent is not None and agent.is_alive():
            try:
                agent.terminate()
                agent.join(timeout=1.0)
            except Exception:
                pass
        # Shut down the top-level manager process.
        try:
            nx.manager.shutdown()
        except Exception:
            pass


# ---------------------------------------------------------------------------
# Client session (one accepted Go connection)
# ---------------------------------------------------------------------------


class ClientSession:
    """Handles a single connection from the Go host process."""

    def __init__(
        self,
        conn: socket.socket,
        controller,
        status_event: threading.Event,
    ) -> None:
        self._conn = conn
        self._controller = controller
        self._status_event = status_event
        self._running = True

    def run(self) -> None:
        log.info("client connected")
        buf = b""
        try:
            while self._running:
                chunk = self._conn.recv(4096)
                if not chunk:
                    break
                buf += chunk
                while b"\n" in buf:
                    line, buf = buf.split(b"\n", 1)
                    line = line.strip()
                    if line:
                        self._handle_line(line)
        except OSError as exc:
            log.debug("client recv error: %s", exc)
        finally:
            log.info("client disconnected")
            try:
                self._conn.close()
            except OSError:
                pass

    def _handle_line(self, line: bytes) -> None:
        try:
            msg = json.loads(line)
        except json.JSONDecodeError as exc:
            log.warning("invalid JSON: %s — %s", exc, line[:120])
            return

        msg_type = msg.get("type")
        if msg_type == "input":
            self._controller.apply_input(
                buttons=int(msg.get("buttons", 0)),
                lx=float(msg.get("lx", 0)),
                ly=float(msg.get("ly", 0)),
                rx=float(msg.get("rx", 0)),
                ry=float(msg.get("ry", 0)),
            )
        elif msg_type == "ping":
            self._send({"type": "pong"})
        else:
            log.warning("unknown message type: %r", msg_type)

    def _send(self, msg: dict) -> None:
        try:
            send_msg(self._conn, msg)
        except OSError as exc:
            log.debug("send error: %s", exc)
            self._running = False

    def send_status(self, state: str, detail: str = "") -> None:
        self._send({"type": "status", "state": state, "detail": detail})


# ---------------------------------------------------------------------------
# Daemon main loop
# ---------------------------------------------------------------------------


class Daemon:
    def __init__(
        self,
        socket_path: str,
        mock: bool,
        skip_bluez_check: bool = False,
        socket_group: Optional[str] = None,
        socket_mode: int = 0o660,
    ) -> None:
        self._socket_path = socket_path
        self._mock = mock or MOCK_MODE
        self._skip_bluez_check = skip_bluez_check
        self._socket_group = socket_group
        self._socket_mode = socket_mode
        self._shutdown = threading.Event()
        self._current_session: Optional[ClientSession] = None
        self._session_lock = threading.Lock()
        self._controller = None
        # Last broadcast status, replayed to clients that connect later so
        # they always learn the current controller state (e.g. they must be
        # able to observe "disconnected" even if the failure predates them).
        self._last_status: tuple[str, str] = ("connecting", "starting")

    # ------------------------------------------------------------------
    # Controller lifecycle with exponential back-off reconnect
    # ------------------------------------------------------------------

    def _make_controller(self):
        if self._mock:
            return MockController()
        return NxbtController()

    def _ensure_bluez_plugin(self) -> None:
        """Gate each real-mode connect attempt on the NUXBT BlueZ plugin.

        The /run systemd override evaporates on reboot, so a daemon started
        at boot (systemd/docker, running as root) must self-heal: write the
        override and bounce bluetoothd. Without root we can only report the
        problem and retry — once the user runs 'sudo nuxbt toggle' the next
        retry proceeds, no daemon restart needed.

        Raises BluezPluginDisabledError (non-root) or CalledProcessError
        (systemctl failure); both surface as "disconnected" + backoff retry.
        """
        bluez = _import_bluez()
        if bluez.is_nuxbt_plugin_enabled():
            return
        if _geteuid() != 0:
            raise BluezPluginDisabledError(PLUGIN_DISABLED_HINT)
        log.info("NUXBT BlueZ plugin not enabled — enabling it (running as root)")
        bluez.toggle_clean_bluez(True)
        # toggle_clean_bluez only writes the override file; the caller must
        # reload systemd and restart bluetoothd for it to take effect.
        for cmd in (["systemctl", "daemon-reload"],
                    ["systemctl", "restart", "bluetooth"]):
            result = subprocess.run(cmd, check=True, capture_output=True, text=True)
            log.info("ran %s (stdout=%r, stderr=%r)",
                     " ".join(cmd), result.stdout.strip(), result.stderr.strip())
        # Give bluetoothd a moment to come back up (cancellable).
        self._shutdown.wait(timeout=BLUETOOTH_RESTART_WAIT)
        log.info("NUXBT BlueZ plugin enabled, bluetoothd restarted")

    def _run_controller(self) -> None:
        """
        Manages the NXBT controller lifecycle in a dedicated thread.
        Retries with exponential back-off on failure.
        """
        backoff = RETRY_BACKOFF_INITIAL
        while not self._shutdown.is_set():
            ctrl = None
            self._broadcast_status("connecting", "creating controller")
            try:
                if not self._mock and not self._skip_bluez_check:
                    self._ensure_bluez_plugin()
                # Controller construction must stay inside the try: with real
                # nuxbt and no usable Bluetooth stack, Nuxbt() itself raises,
                # and that must surface as a "disconnected" status + retry,
                # never as an unhandled exception killing this thread.
                ctrl = self._make_controller()
                if not self._mock:
                    ctrl.connect(self._shutdown)
                with self._session_lock:
                    self._controller = ctrl
                self._broadcast_status("connected", "Switch paired" if not self._mock else "mock mode")
                backoff = RETRY_BACKOFF_INITIAL  # reset on success
                # Keep the controller alive until shutdown or error.
                while not self._shutdown.is_set():
                    time.sleep(1)
                break
            except Exception as exc:
                log.error("controller error: %s — retrying in %.0fs", exc, backoff)
                self._broadcast_status("disconnected", str(exc))
                if self._shutdown.wait(timeout=backoff):
                    break
                backoff = min(backoff * 2, RETRY_BACKOFF_MAX)
            finally:
                with self._session_lock:
                    self._controller = None
                if ctrl is not None:
                    ctrl.close()

    def _broadcast_status(self, state: str, detail: str = "") -> None:
        with self._session_lock:
            self._last_status = (state, detail)
            session = self._current_session
        if session is not None:
            session.send_status(state, detail)

    # ------------------------------------------------------------------
    # Socket server
    # ------------------------------------------------------------------

    def _apply_socket_permissions(self) -> None:
        """Make the socket connectable by the (non-root) playgate-host user.

        Connecting to a UNIX socket requires WRITE permission on the socket
        inode; under root with the default umask the socket comes out 0755,
        which silently locks every other user out. Group + 0660 is the
        intended deployment: nxbtd runs as root, playgate-host as the
        'playgate' user, and the socket is root:<group> rw-rw----.
        """
        if self._socket_group is not None:
            try:
                shutil.chown(self._socket_path, group=self._socket_group)
            except (LookupError, PermissionError, OSError) as exc:
                # Deployment problem, not a reason to refuse service to a
                # root client: report loudly and keep running.
                log.warning(
                    "could not set socket group %r on %s: %s — non-root "
                    "clients will be unable to connect",
                    self._socket_group, self._socket_path, exc,
                )
        os.chmod(self._socket_path, self._socket_mode)

    def run(self) -> None:
        # Remove stale socket file.
        try:
            os.unlink(self._socket_path)
        except FileNotFoundError:
            pass

        # Ensure parent directory exists.
        os.makedirs(os.path.dirname(os.path.abspath(self._socket_path)), exist_ok=True)

        server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        server.bind(self._socket_path)
        self._apply_socket_permissions()
        server.listen(1)
        server.settimeout(1.0)  # allow periodic shutdown checks
        log.info("listening on %s (mock=%s)", self._socket_path, self._mock)

        ctrl_thread = threading.Thread(target=self._run_controller, daemon=True, name="controller")
        ctrl_thread.start()

        try:
            while not self._shutdown.is_set():
                try:
                    conn, _ = server.accept()
                except socket.timeout:
                    continue

                # Only one client at a time; close any existing session.
                with self._session_lock:
                    old = self._current_session
                if old is not None:
                    try:
                        old._conn.close()
                    except OSError:
                        pass

                # Get the active controller reference (either real NxbtController or MockController)
                with self._session_lock:
                    session_ctrl = self._controller if self._controller is not None else MockController()
                session = ClientSession(conn, session_ctrl, self._shutdown)
                with self._session_lock:
                    self._current_session = session
                    last_state, last_detail = self._last_status

                # Replay the current controller state so a client connecting
                # after a status change (e.g. an earlier Bluetooth failure)
                # still learns it immediately.
                session.send_status(last_state, last_detail)

                t = threading.Thread(target=session.run, daemon=True, name="session")
                t.start()
        finally:
            server.close()
            try:
                os.unlink(self._socket_path)
            except FileNotFoundError:
                pass
            self._shutdown.set()
            ctrl_thread.join(timeout=5)
            # Mop up any multiprocessing children the controller thread's
            # close() did not reap (e.g. it was wedged past the join above).
            _cleanup_children()
            log.info("daemon stopped")

    def stop(self) -> None:
        self._shutdown.set()


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------

# Grace period for the whole shutdown path (controller close, child joins,
# nuxbt atexit cleanup) before the watchdog force-exits the process.
SHUTDOWN_GRACE_SECONDS = 10.0


def _child_signals_after_fork(_obj=None) -> None:
    """Set safe SIGINT/SIGTERM dispositions in forked children.

    nuxbt spawns several multiprocessing children (controller manager, BlueZ
    agent, manager processes). They fork after main() installed our handlers,
    so without this hook every child inherits a handler that merely sets the
    parent daemon's shutdown event — i.e. the children silently swallow the
    SIGTERM that multiprocessing's terminate()/atexit cleanup relies on, and
    the daemon hangs forever on exit.

    SIGINT → SIG_IGN: a terminal Ctrl-C delivers SIGINT to the whole process
    group. Children must NOT react to it — the parent orchestrates shutdown.
    (With SIG_DFL, the group SIGINT killed nuxbt's controller-manager process
    instantly, before its graceful cleanup could run, orphaning its own
    children: the controller session processes and its inner Manager process,
    which itself ignores SIGINT per bpo-36368 and therefore lingered forever.)

    SIGTERM → SIG_DFL: Process.terminate() and multiprocessing's atexit
    cleanup must actually kill children. This hook runs in Process._bootstrap
    BEFORE the child's target function, so a child that installs its own
    SIGTERM handler (nuxbt's _command_manager: SIGTERM → sys.exit(0), whose
    finally-block reaps its grandchildren) still wins.
    """
    signal.signal(signal.SIGINT, signal.SIG_IGN)
    signal.signal(signal.SIGTERM, signal.SIG_DFL)


def _cleanup_children(children=None, grace: float = 2.0) -> None:
    """Terminate any still-alive direct multiprocessing children.

    terminate (SIGTERM) → join with a shared deadline → kill (SIGKILL)
    stragglers. Normally a no-op: NxbtController.close() already shut
    everything down gracefully. `children` is injectable for tests; defaults
    to multiprocessing.active_children().
    """
    if children is None:
        children = multiprocessing.active_children()
    children = [child for child in children if child.is_alive()]
    if not children:
        return
    log.warning("cleaning up %d leftover child process(es): %s",
                len(children), [child.name for child in children])
    for child in children:
        try:
            child.terminate()
        except Exception:
            pass
    deadline = time.monotonic() + grace
    for child in children:
        child.join(timeout=max(0.0, deadline - time.monotonic()))
    for child in children:
        if child.is_alive():
            log.warning("child %s ignored SIGTERM — killing", child.name)
            try:
                child.kill()
                child.join(timeout=1.0)
            except Exception:
                pass


def _build_arg_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="PlayGate NXBT daemon")
    parser.add_argument(
        "--socket",
        default="/run/nxbt/nxbt.sock",
        help="Unix socket path (default: /run/nxbt/nxbt.sock)",
    )
    parser.add_argument(
        "--mock",
        action="store_true",
        help="Force mock mode (no Bluetooth, log inputs to stdout)",
    )
    parser.add_argument(
        "--skip-bluez-check",
        action="store_true",
        help="Skip the NUXBT BlueZ plugin gate before each connect attempt "
             "(for testing, or setups where the systemd override cannot be "
             "installed)",
    )
    parser.add_argument(
        "--socket-group",
        default=None,
        help="Group to own the listening socket (e.g. 'playgate') so a "
             "non-root playgate-host can connect to a root-run daemon",
    )
    parser.add_argument(
        "--socket-mode",
        type=lambda s: int(s, 8),
        default=0o660,
        help="Octal permissions for the listening socket (default: 660)",
    )
    parser.add_argument(
        "--debug",
        action="store_true",
        help="Enable debug logging",
    )
    return parser


def main() -> None:
    args = _build_arg_parser().parse_args()

    if args.debug:
        logging.getLogger().setLevel(logging.DEBUG)

    daemon = Daemon(
        socket_path=args.socket,
        mock=args.mock,
        skip_bluez_check=args.skip_bluez_check,
        socket_group=args.socket_group,
        socket_mode=args.socket_mode,
    )

    watchdog_armed = threading.Event()

    def _handle_signal(signum, frame):
        log.info("received signal %d, shutting down…", signum)
        daemon.stop()
        # Belt and braces: arm a last-resort watchdog on the FIRST signal so
        # it covers the entire shutdown path (child joins, atexit cleanup).
        # The normal path exits well before this fires.
        if not watchdog_armed.is_set():
            watchdog_armed.set()
            watchdog = threading.Timer(SHUTDOWN_GRACE_SECONDS, os._exit, args=(0,))
            watchdog.daemon = True
            watchdog.start()

    signal.signal(signal.SIGINT, _handle_signal)
    signal.signal(signal.SIGTERM, _handle_signal)

    # Ensure forked children (nuxbt's multiprocessing workers) do not inherit
    # the handlers above — see _child_signals_after_fork.
    multiprocessing.util.register_after_fork(
        _child_signals_after_fork, _child_signals_after_fork
    )

    daemon.run()


if __name__ == "__main__":
    main()
