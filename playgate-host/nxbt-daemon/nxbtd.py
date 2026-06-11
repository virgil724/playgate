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
import os
import signal
import socket
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
# nuxbt import — fall back to mock mode gracefully
# ---------------------------------------------------------------------------

MOCK_MODE = False

try:
    import nuxbt  # type: ignore
    _nuxbt_available = True
except ImportError:
    _nuxbt_available = False
    MOCK_MODE = True
    logging.warning(
        "nuxbt module not found — running in mock mode. "
        "Inputs will be logged but not forwarded to a real Switch."
    )

# ---------------------------------------------------------------------------
# Logging setup
# ---------------------------------------------------------------------------

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)-8s %(name)s %(message)s",
    datefmt="%Y-%m-%dT%H:%M:%S",
)
log = logging.getLogger("nxbtd")

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


class NxbtController:
    """Wraps the real nuxbt library."""

    def __init__(self) -> None:
        self._nx = nuxbt.Nuxbt()
        self._index: Optional[int] = None

    def connect(self) -> None:
        """Create a Pro Controller and wait for the Switch to pair."""
        self._index = self._nx.create_controller(nuxbt.PRO_CONTROLLER)
        log.info("nuxbt controller index=%d, waiting for Switch…", self._index)
        self._nx.wait_for_connection(self._index)
        log.info("Switch paired to controller index=%d", self._index)

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
    def __init__(self, socket_path: str, mock: bool) -> None:
        self._socket_path = socket_path
        self._mock = mock or MOCK_MODE
        self._shutdown = threading.Event()
        self._current_session: Optional[ClientSession] = None
        self._session_lock = threading.Lock()

    # ------------------------------------------------------------------
    # Controller lifecycle with exponential back-off reconnect
    # ------------------------------------------------------------------

    def _make_controller(self):
        if self._mock:
            return MockController()
        return NxbtController()

    def _run_controller(self) -> None:
        """
        Manages the NXBT controller lifecycle in a dedicated thread.
        Retries with exponential back-off on failure.
        """
        backoff = 2.0
        while not self._shutdown.is_set():
            ctrl = self._make_controller()
            self._broadcast_status("connecting", "creating controller")
            try:
                if not self._mock:
                    ctrl.connect()
                self._broadcast_status("connected", "Switch paired" if not self._mock else "mock mode")
                backoff = 2.0  # reset on success
                # Keep the controller alive until shutdown or error.
                while not self._shutdown.is_set():
                    time.sleep(1)
                break
            except Exception as exc:
                log.error("controller error: %s — retrying in %.0fs", exc, backoff)
                self._broadcast_status("disconnected", str(exc))
                ctrl.close()
                if self._shutdown.wait(timeout=backoff):
                    break
                backoff = min(backoff * 2, 60.0)
            finally:
                ctrl.close()

    def _broadcast_status(self, state: str, detail: str = "") -> None:
        with self._session_lock:
            session = self._current_session
        if session is not None:
            session.send_status(state, detail)

    # ------------------------------------------------------------------
    # Socket server
    # ------------------------------------------------------------------

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

                # Build a mock controller for the session in mock mode so that
                # apply_input calls are routed through it.  In real mode we
                # want to share the single NxbtController, but _run_controller
                # holds it privately; for now pass a fresh MockController so the
                # session can forward inputs without blocking.
                # TODO(T16): wire real controller reference into session.
                session_ctrl = MockController() if self._mock else MockController()
                session = ClientSession(conn, session_ctrl, self._shutdown)
                with self._session_lock:
                    self._current_session = session

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
            log.info("daemon stopped")

    def stop(self) -> None:
        self._shutdown.set()


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------


def main() -> None:
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
        "--debug",
        action="store_true",
        help="Enable debug logging",
    )
    args = parser.parse_args()

    if args.debug:
        logging.getLogger().setLevel(logging.DEBUG)

    daemon = Daemon(socket_path=args.socket, mock=args.mock)

    def _handle_signal(signum, frame):
        log.info("received signal %d, shutting down…", signum)
        daemon.stop()

    signal.signal(signal.SIGINT, _handle_signal)
    signal.signal(signal.SIGTERM, _handle_signal)

    daemon.run()


if __name__ == "__main__":
    main()
