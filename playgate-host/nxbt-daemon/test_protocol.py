"""
test_protocol.py — Unit tests for the nxbtd protocol layer.

Does NOT import nuxbt.  Runs on any platform (Linux, macOS, Windows).
Uses only the Python standard library.

Run with:
    python3 test_protocol.py
or:
    python3 -m pytest test_protocol.py -v
"""

import io
import json
import os
import socket
import sys
import tempfile
import threading
import time
import unittest

# ---------------------------------------------------------------------------
# Import protocol helpers from nxbtd without triggering a real nuxbt import.
# nxbtd already handles `import nuxbt` failing gracefully by setting MOCK_MODE,
# so a plain `import nxbtd` is safe on non-Linux hosts.
# ---------------------------------------------------------------------------
import nxbtd


# ---------------------------------------------------------------------------
# Cross-platform socket pair helper
#
# AF_UNIX socketpair is available on Linux/macOS but not on Windows.
# We provide a portable alternative using a loopback TCP socket pair.
# ---------------------------------------------------------------------------

def _make_socketpair():
    """Return (a, b) where a and b are connected sockets.

    Uses AF_UNIX socketpair on Linux/macOS and a TCP loopback pair on Windows.
    """
    if hasattr(socket, 'AF_UNIX') and hasattr(socket, 'socketpair'):
        try:
            return socket.socketpair(socket.AF_UNIX, socket.SOCK_STREAM)
        except OSError:
            pass  # fall through to TCP fallback

    # TCP loopback pair: create a listener, connect, accept, close listener.
    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.bind(('127.0.0.1', 0))
    srv.listen(1)
    _, port = srv.getsockname()
    a = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    a.connect(('127.0.0.1', port))
    b, _ = srv.accept()
    srv.close()
    return a, b


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

class _RecordingController:
    """Minimal controller stub that records every apply_input call."""

    def __init__(self):
        self.calls = []

    def apply_input(self, buttons, lx, ly, rx, ry):
        self.calls.append(dict(buttons=buttons, lx=lx, ly=ly, rx=rx, ry=ry))

    def close(self):
        pass


def _make_session(ctrl=None):
    """Build a ClientSession backed by a connected socket pair."""
    ctrl = ctrl or _RecordingController()
    a, b = _make_socketpair()
    session = nxbtd.ClientSession(conn=b, controller=ctrl, status_event=threading.Event())
    return session, a, b, ctrl


# ---------------------------------------------------------------------------
# Test: send_msg
# ---------------------------------------------------------------------------

class TestSendMsg(unittest.TestCase):

    def test_send_msg_json_terminated(self):
        """send_msg writes compact JSON followed by a newline."""
        a, b = _make_socketpair()
        try:
            nxbtd.send_msg(a, {"type": "pong"})
            b.settimeout(1.0)
            data = b.recv(256)
        finally:
            a.close()
            b.close()

        self.assertTrue(data.endswith(b'\n'), "message must end with newline")
        parsed = json.loads(data.decode().strip())
        self.assertEqual(parsed, {"type": "pong"})

    def test_send_msg_no_extra_whitespace(self):
        """send_msg uses compact separators (no extra spaces)."""
        a, b = _make_socketpair()
        try:
            nxbtd.send_msg(a, {"type": "status", "state": "connected"})
            b.settimeout(1.0)
            data = b.recv(256)
        finally:
            a.close()
            b.close()

        line = data.decode().strip()
        self.assertNotIn(': ', line, "compact JSON must not have ': '")
        self.assertNotIn(', ', line, "compact JSON must not have ', '")


# ---------------------------------------------------------------------------
# Test: buttons_to_nxbt
# ---------------------------------------------------------------------------

class TestButtonsToNxbt(unittest.TestCase):

    def test_single_button(self):
        result = nxbtd.buttons_to_nxbt(0x000001)  # ButtonA
        self.assertEqual(result, ["A"])

    def test_multiple_buttons(self):
        # ButtonA | ButtonB | ButtonDpadUp
        mask = 0x000001 | 0x000002 | 0x004000
        result = nxbtd.buttons_to_nxbt(mask)
        self.assertIn("A", result)
        self.assertIn("B", result)
        self.assertIn("DPAD_UP", result)
        self.assertEqual(len(result), 3)

    def test_no_buttons(self):
        self.assertEqual(nxbtd.buttons_to_nxbt(0), [])

    def test_all_buttons(self):
        all_mask = sum(mask for mask, _ in nxbtd.BUTTON_MAP)
        result = nxbtd.buttons_to_nxbt(all_mask)
        self.assertEqual(len(result), len(nxbtd.BUTTON_MAP))

    def test_full_bitmask_table(self):
        """Every entry in BUTTON_MAP maps its mask to exactly its name."""
        for mask, name in nxbtd.BUTTON_MAP:
            result = nxbtd.buttons_to_nxbt(mask)
            self.assertEqual(result, [name], f"mask={mask:#010x} name={name!r}")

    def test_bitmask_consistency_with_core_types(self):
        """
        Verify the button bitmask layout matches the documented bit assignments
        in internal/core/types.go (bit 0 = A, bit 1 = B, …, bit 17 = DPAD_RIGHT).
        """
        expected_order = [
            (0,  "A"),
            (1,  "B"),
            (2,  "X"),
            (3,  "Y"),
            (4,  "L"),
            (5,  "R"),
            (6,  "ZL"),
            (7,  "ZR"),
            (8,  "PLUS"),
            (9,  "MINUS"),
            (10, "HOME"),
            (11, "CAPTURE"),
            (12, "L_STICK"),
            (13, "R_STICK"),
            (14, "DPAD_UP"),
            (15, "DPAD_DOWN"),
            (16, "DPAD_LEFT"),
            (17, "DPAD_RIGHT"),
        ]
        for bit, expected_name in expected_order:
            mask = 1 << bit
            # Find the entry in BUTTON_MAP with this mask.
            matching = [name for m, name in nxbtd.BUTTON_MAP if m == mask]
            self.assertEqual(len(matching), 1,
                             f"bit {bit} has {len(matching)} entries in BUTTON_MAP")
            self.assertEqual(
                matching[0], expected_name,
                f"bit {bit} (mask={mask:#010x}): got {matching[0]!r}, want {expected_name!r}",
            )


# ---------------------------------------------------------------------------
# Test: axis_to_nxbt
# ---------------------------------------------------------------------------

class TestAxisToNxbt(unittest.TestCase):

    def test_centre(self):
        self.assertEqual(nxbtd.axis_to_nxbt(0.0), 0)

    def test_full_positive(self):
        self.assertEqual(nxbtd.axis_to_nxbt(1.0), 100)

    def test_full_negative(self):
        self.assertEqual(nxbtd.axis_to_nxbt(-1.0), -100)

    def test_half(self):
        self.assertEqual(nxbtd.axis_to_nxbt(0.5), 50)

    def test_clamp_above(self):
        self.assertEqual(nxbtd.axis_to_nxbt(2.0), 100)

    def test_clamp_below(self):
        self.assertEqual(nxbtd.axis_to_nxbt(-2.0), -100)

    def test_rounding(self):
        # 0.755 * 100 = 75.5 → should round to 76.
        self.assertEqual(nxbtd.axis_to_nxbt(0.755), 76)


# ---------------------------------------------------------------------------
# Test: fill_input_packet (nuxbt DIRECT_INPUT_PACKET mapping)
# ---------------------------------------------------------------------------

def _make_packet():
    """Mirror of nuxbt's DIRECT_INPUT_PACKET template (nuxbt/nuxbt.py).

    Kept local so the tests never import nuxbt.
    """
    return {
        "L_STICK": {"PRESSED": False, "X_VALUE": 0, "Y_VALUE": 0,
                    "LS_UP": False, "LS_LEFT": False, "LS_RIGHT": False, "LS_DOWN": False},
        "R_STICK": {"PRESSED": False, "X_VALUE": 0, "Y_VALUE": 0,
                    "RS_UP": False, "RS_LEFT": False, "RS_RIGHT": False, "RS_DOWN": False},
        "DPAD_UP": False, "DPAD_LEFT": False, "DPAD_RIGHT": False, "DPAD_DOWN": False,
        "L": False, "ZL": False, "R": False, "ZR": False,
        "JCL_SR": False, "JCL_SL": False, "JCR_SR": False, "JCR_SL": False,
        "PLUS": False, "MINUS": False, "HOME": False, "CAPTURE": False,
        "Y": False, "X": False, "B": False, "A": False,
    }


class TestFillInputPacket(unittest.TestCase):

    def test_neutral(self):
        pkt = nxbtd.fill_input_packet(_make_packet(), 0, 0.0, 0.0, 0.0, 0.0)
        self.assertEqual(pkt, _make_packet())

    def test_face_buttons(self):
        # A | B | X | Y
        pkt = nxbtd.fill_input_packet(_make_packet(), 0x0F, 0, 0, 0, 0)
        for name in ("A", "B", "X", "Y"):
            self.assertTrue(pkt[name], name)
        self.assertFalse(pkt["L"])

    def test_stick_clicks_map_to_pressed(self):
        # ButtonLStick (bit 12) | ButtonRStick (bit 13)
        pkt = nxbtd.fill_input_packet(_make_packet(), 0x001000 | 0x002000, 0, 0, 0, 0)
        self.assertTrue(pkt["L_STICK"]["PRESSED"])
        self.assertTrue(pkt["R_STICK"]["PRESSED"])
        # Must NOT create stray top-level keys.
        self.assertNotIn("L_STICK_PRESS", pkt)

    def test_dpad(self):
        pkt = nxbtd.fill_input_packet(_make_packet(), 0x004000 | 0x020000, 0, 0, 0, 0)
        self.assertTrue(pkt["DPAD_UP"])
        self.assertTrue(pkt["DPAD_RIGHT"])
        self.assertFalse(pkt["DPAD_DOWN"])
        self.assertFalse(pkt["DPAD_LEFT"])

    def test_plus_minus_map_directly(self):
        # The fork fixes nuxbt's direct-input PLUS/MINUS swap, so the daemon no
        # longer compensates: wire bits map straight through to matching keys.
        pkt = nxbtd.fill_input_packet(_make_packet(), 0x000100, 0, 0, 0, 0)  # ButtonPlus
        self.assertTrue(pkt["PLUS"])
        self.assertFalse(pkt["MINUS"])
        pkt = nxbtd.fill_input_packet(_make_packet(), 0x000200, 0, 0, 0, 0)  # ButtonMinus
        self.assertTrue(pkt["MINUS"])
        self.assertFalse(pkt["PLUS"])

    def test_axes_scaled_to_nuxbt_range(self):
        pkt = nxbtd.fill_input_packet(_make_packet(), 0, 0.5, -1.0, 0.25, 2.0)
        self.assertEqual(pkt["L_STICK"]["X_VALUE"], 50)
        self.assertEqual(pkt["L_STICK"]["Y_VALUE"], -100)
        self.assertEqual(pkt["R_STICK"]["X_VALUE"], 25)
        self.assertEqual(pkt["R_STICK"]["Y_VALUE"], 100)  # clamped

    def test_all_buttons_only_touch_known_keys(self):
        all_mask = (1 << 18) - 1
        pkt = nxbtd.fill_input_packet(_make_packet(), all_mask, 0, 0, 0, 0)
        self.assertEqual(set(pkt.keys()), set(_make_packet().keys()))
        for name in ("A", "B", "X", "Y", "L", "R", "ZL", "ZR",
                     "PLUS", "MINUS", "HOME", "CAPTURE",
                     "DPAD_UP", "DPAD_DOWN", "DPAD_LEFT", "DPAD_RIGHT"):
            self.assertTrue(pkt[name], name)
        self.assertTrue(pkt["L_STICK"]["PRESSED"])
        self.assertTrue(pkt["R_STICK"]["PRESSED"])
        # Joy-Con-only buttons stay untouched on a Pro Controller.
        for name in ("JCL_SR", "JCL_SL", "JCR_SR", "JCR_SL"):
            self.assertFalse(pkt[name], name)


# ---------------------------------------------------------------------------
# Test: ClientSession._handle_line
# ---------------------------------------------------------------------------

class TestClientSessionHandleLine(unittest.TestCase):

    def _make(self):
        ctrl = _RecordingController()
        a, b = _make_socketpair()
        a.settimeout(1.0)
        session = nxbtd.ClientSession(conn=b, controller=ctrl, status_event=threading.Event())
        return session, a, b, ctrl

    def test_input_message_forwarded(self):
        session, a, b, ctrl = self._make()
        try:
            msg = b'{"type":"input","buttons":3,"lx":0.5,"ly":-0.5,"rx":0.0,"ry":0.1}'
            session._handle_line(msg)
        finally:
            a.close(); b.close()

        self.assertEqual(len(ctrl.calls), 1)
        call = ctrl.calls[0]
        self.assertEqual(call["buttons"], 3)
        self.assertAlmostEqual(call["lx"], 0.5)
        self.assertAlmostEqual(call["ly"], -0.5)
        self.assertAlmostEqual(call["rx"], 0.0)
        self.assertAlmostEqual(call["ry"], 0.1)

    def test_ping_returns_pong(self):
        session, a, b, ctrl = self._make()
        try:
            session._handle_line(b'{"type":"ping"}')
            data = a.recv(256)
        finally:
            a.close(); b.close()

        parsed = json.loads(data.decode().strip())
        self.assertEqual(parsed, {"type": "pong"})

    def test_input_defaults_to_zero(self):
        """Omitted axis fields default to 0."""
        session, a, b, ctrl = self._make()
        try:
            session._handle_line(b'{"type":"input","buttons":0}')
        finally:
            a.close(); b.close()

        self.assertEqual(len(ctrl.calls), 1)
        call = ctrl.calls[0]
        self.assertEqual(call["lx"], 0.0)
        self.assertEqual(call["ly"], 0.0)

    def test_unknown_type_ignored(self):
        session, a, b, ctrl = self._make()
        try:
            session._handle_line(b'{"type":"bogus"}')
        finally:
            a.close(); b.close()

        self.assertEqual(ctrl.calls, [])

    def test_invalid_json_ignored(self):
        session, a, b, ctrl = self._make()
        try:
            session._handle_line(b'not json at all')
        finally:
            a.close(); b.close()

        self.assertEqual(ctrl.calls, [])

    def test_input_zero_buttons(self):
        """buttons=0 (all released) must be forwarded without error."""
        session, a, b, ctrl = self._make()
        try:
            session._handle_line(b'{"type":"input","buttons":0,"lx":0,"ly":0,"rx":0,"ry":0}')
        finally:
            a.close(); b.close()

        self.assertEqual(len(ctrl.calls), 1)
        self.assertEqual(ctrl.calls[0]["buttons"], 0)

    def test_all_buttons_mask(self):
        """Full 18-bit button mask is forwarded correctly."""
        all_mask = (1 << 18) - 1  # bits 0-17
        session, a, b, ctrl = self._make()
        try:
            msg = json.dumps({
                "type": "input", "buttons": all_mask,
                "lx": 0, "ly": 0, "rx": 0, "ry": 0,
            }).encode()
            session._handle_line(msg)
        finally:
            a.close(); b.close()

        self.assertEqual(ctrl.calls[0]["buttons"], all_mask)

    def test_input_latency_sample_reported_every_30_inputs(self):
        session, a, b, ctrl = self._make()
        try:
            session._input_lat_samples = 29
            session._handle_line(b'{"type":"input","buttons":1}')
            data = a.recv(256)
        finally:
            a.close(); b.close()

        parsed = json.loads(data.decode().strip())
        self.assertEqual(parsed["type"], "input_lat")
        self.assertIsInstance(parsed["us"], int)
        self.assertGreaterEqual(parsed["us"], 0)
        self.assertEqual(len(ctrl.calls), 1)


# ---------------------------------------------------------------------------
# Test: send_status (ClientSession helper)
# ---------------------------------------------------------------------------

class TestSendStatus(unittest.TestCase):

    def test_send_status_connected(self):
        a, b = _make_socketpair()
        ctrl = _RecordingController()
        session = nxbtd.ClientSession(conn=b, controller=ctrl, status_event=threading.Event())
        try:
            session.send_status("connected", "Switch paired")
            a.settimeout(1.0)
            data = a.recv(256)
        finally:
            a.close(); b.close()

        parsed = json.loads(data.decode().strip())
        self.assertEqual(parsed["type"], "status")
        self.assertEqual(parsed["state"], "connected")
        self.assertEqual(parsed["detail"], "Switch paired")

    def test_send_status_no_detail(self):
        a, b = _make_socketpair()
        ctrl = _RecordingController()
        session = nxbtd.ClientSession(conn=b, controller=ctrl, status_event=threading.Event())
        try:
            session.send_status("disconnected")
            a.settimeout(1.0)
            data = a.recv(256)
        finally:
            a.close(); b.close()

        parsed = json.loads(data.decode().strip())
        self.assertEqual(parsed["state"], "disconnected")
        # detail should be empty string — both absent and "" are acceptable.
        self.assertIn(parsed.get("detail", ""), ("", None))


# ---------------------------------------------------------------------------
# Test: BUTTON_MAP ordering sanity
# ---------------------------------------------------------------------------

class TestButtonMapStructure(unittest.TestCase):

    def test_unique_masks(self):
        masks = [mask for mask, _ in nxbtd.BUTTON_MAP]
        self.assertEqual(len(masks), len(set(masks)), "BUTTON_MAP must have unique masks")

    def test_unique_names(self):
        names = [name for _, name in nxbtd.BUTTON_MAP]
        self.assertEqual(len(names), len(set(names)), "BUTTON_MAP must have unique names")

    def test_all_masks_are_powers_of_two(self):
        for mask, name in nxbtd.BUTTON_MAP:
            self.assertTrue(
                mask > 0 and (mask & (mask - 1)) == 0,
                f"{name!r}: mask {mask:#x} is not a power of two",
            )

    def test_18_entries(self):
        self.assertEqual(
            len(nxbtd.BUTTON_MAP), 18,
            "expected 18 button entries matching core.Button* constants",
        )


# ---------------------------------------------------------------------------
# Test: Daemon controller-thread failure handling (real-mode robustness)
# ---------------------------------------------------------------------------

class _RecordingSession:
    """Stands in for ClientSession; records send_status calls."""

    def __init__(self, on_status=None):
        self.statuses = []
        self._on_status = on_status

    def send_status(self, state, detail=""):
        self.statuses.append((state, detail))
        if self._on_status is not None:
            self._on_status(state, detail)


class TestDaemonControllerFailure(unittest.TestCase):
    """The controller thread must never die with an unhandled exception.

    Regression test: with real nuxbt and no usable Bluetooth stack,
    Nuxbt() itself raises inside _make_controller(). That must surface as
    a "disconnected" status and a retry — not kill the thread.
    """

    def test_make_controller_failure_reports_disconnected(self):
        daemon = nxbtd.Daemon(socket_path="/tmp/unused-test.sock", mock=False)

        # Stop the daemon as soon as the failure has been reported so the
        # retry loop exits instead of sleeping through its backoff.
        session = _RecordingSession(
            on_status=lambda state, detail: daemon.stop() if state == "disconnected" else None
        )
        daemon._current_session = session

        def boom():
            raise RuntimeError("no bluetooth adapter")
        daemon._make_controller = boom

        # Must not raise, and must finish promptly once stop() is called.
        t = threading.Thread(target=daemon._run_controller)
        t.start()
        t.join(timeout=10)
        self.assertFalse(t.is_alive(), "_run_controller must exit after stop()")

        self.assertIn(("connecting", "creating controller"), session.statuses)
        self.assertIn(("disconnected", "no bluetooth adapter"), session.statuses)

    def test_last_status_tracks_broadcasts(self):
        """_broadcast_status must record the state for replay to late clients."""
        daemon = nxbtd.Daemon(socket_path="/tmp/unused-test.sock", mock=False)
        self.assertEqual(daemon._last_status, ("connecting", "starting"))
        daemon._broadcast_status("disconnected", "boom")
        self.assertEqual(daemon._last_status, ("disconnected", "boom"))


# ---------------------------------------------------------------------------
# Test: forked children must not inherit the daemon's signal handlers
# ---------------------------------------------------------------------------

class TestChildSignalsAfterFork(unittest.TestCase):
    """Regression tests for the real-mode shutdown hang.

    Children must IGNORE SIGINT: a terminal Ctrl-C hits the whole process
    group, and with SIG_DFL it killed nuxbt's controller-manager process
    instantly — before its graceful cleanup could reap its own children
    (controller sessions + an inner Manager process that itself ignores
    SIGINT per bpo-36368), leaving orphans on init.

    Children must take the DEFAULT action on SIGTERM so Process.terminate()
    and multiprocessing's atexit cleanup actually kill them (the inherited
    parent handler merely set the parent's shutdown event)."""

    def test_child_dispositions(self):
        import signal as _signal

        old_int = _signal.getsignal(_signal.SIGINT)
        old_term = _signal.getsignal(_signal.SIGTERM)
        try:
            dummy = lambda signum, frame: None
            _signal.signal(_signal.SIGINT, dummy)
            _signal.signal(_signal.SIGTERM, dummy)

            nxbtd._child_signals_after_fork()

            self.assertEqual(_signal.getsignal(_signal.SIGINT), _signal.SIG_IGN)
            self.assertEqual(_signal.getsignal(_signal.SIGTERM), _signal.SIG_DFL)
        finally:
            _signal.signal(_signal.SIGINT, old_int)
            _signal.signal(_signal.SIGTERM, old_term)


# ---------------------------------------------------------------------------
# Test: _cleanup_children (terminate → join → kill escalation)
# ---------------------------------------------------------------------------

class _FakeChild:
    """Stands in for a multiprocessing.Process child."""

    def __init__(self, name, dies_on_terminate=True):
        self.name = name
        self._alive = True
        self._dies_on_terminate = dies_on_terminate
        self.calls = []

    def is_alive(self):
        return self._alive

    def terminate(self):
        self.calls.append("terminate")
        if self._dies_on_terminate:
            self._alive = False

    def join(self, timeout=None):
        self.calls.append("join")

    def kill(self):
        self.calls.append("kill")
        self._alive = False


class TestCleanupChildren(unittest.TestCase):

    def test_terminates_then_joins(self):
        child = _FakeChild("controllers")
        nxbtd._cleanup_children(children=[child], grace=0.1)
        self.assertEqual(child.calls[:2], ["terminate", "join"])
        self.assertNotIn("kill", child.calls, "no SIGKILL when SIGTERM worked")

    def test_escalates_to_kill_when_terminate_ignored(self):
        child = _FakeChild("stubborn", dies_on_terminate=False)
        nxbtd._cleanup_children(children=[child], grace=0.1)
        self.assertEqual(child.calls, ["terminate", "join", "kill", "join"])
        self.assertFalse(child.is_alive())

    def test_skips_already_dead_children(self):
        child = _FakeChild("dead")
        child._alive = False
        nxbtd._cleanup_children(children=[child], grace=0.1)
        self.assertEqual(child.calls, [])

    def test_default_uses_active_children(self):
        """Without an explicit list it must consult active_children() —
        an empty process pool means a clean no-op."""
        nxbtd._cleanup_children(grace=0.1)  # must not raise

    def test_real_child_is_reaped(self):
        """End-to-end: a live multiprocessing child is terminated."""
        import multiprocessing as mp
        proc = mp.Process(target=_sleep_forever)
        proc.start()
        try:
            self.assertTrue(proc.is_alive())
            nxbtd._cleanup_children(grace=5.0)
            self.assertFalse(proc.is_alive(), "child must be reaped")
        finally:
            if proc.is_alive():
                proc.kill()
            proc.join(timeout=5.0)


def _sleep_forever():
    import time as _time
    _time.sleep(300)


# ---------------------------------------------------------------------------
# Test: BlueZ plugin gate (real mode; nuxbt.bluez is faked — never imported)
# ---------------------------------------------------------------------------

class _FakeBluez:
    """Stands in for the lazily imported nuxbt.bluez module."""

    def __init__(self, enabled=False):
        self.enabled = enabled
        self.enabled_checks = 0
        self.toggle_calls = []

    def is_nuxbt_plugin_enabled(self):
        self.enabled_checks += 1
        return self.enabled

    def toggle_clean_bluez(self, toggle):
        self.toggle_calls.append(toggle)
        self.enabled = bool(toggle)


class _FakeCompletedProcess:
    stdout = ""
    stderr = ""


class _GateHarness(unittest.TestCase):
    """Shared monkeypatching for the plugin-gate tests."""

    def setUp(self):
        self._saved = {
            "_import_bluez": nxbtd._import_bluez,
            "_geteuid": nxbtd._geteuid,
            "RETRY_BACKOFF_INITIAL": nxbtd.RETRY_BACKOFF_INITIAL,
            "BLUETOOTH_RESTART_WAIT": nxbtd.BLUETOOTH_RESTART_WAIT,
            "subprocess_run": nxbtd.subprocess.run,
        }
        # Keep retries fast in tests.
        nxbtd.RETRY_BACKOFF_INITIAL = 0.01
        nxbtd.BLUETOOTH_RESTART_WAIT = 0.01

    def tearDown(self):
        nxbtd._import_bluez = self._saved["_import_bluez"]
        nxbtd._geteuid = self._saved["_geteuid"]
        nxbtd.RETRY_BACKOFF_INITIAL = self._saved["RETRY_BACKOFF_INITIAL"]
        nxbtd.BLUETOOTH_RESTART_WAIT = self._saved["BLUETOOTH_RESTART_WAIT"]
        nxbtd.subprocess.run = self._saved["subprocess_run"]

    def _make_daemon(self, skip_bluez_check=False):
        daemon = nxbtd.Daemon(
            socket_path="/tmp/unused-test.sock",
            mock=False,
            skip_bluez_check=skip_bluez_check,
        )
        # In CI nxbtd.MOCK_MODE is True (nuxbt absent), which forces _mock on
        # and would bypass the gate. These tests exercise the REAL-mode
        # supervision path with every nuxbt touchpoint faked out.
        daemon._mock = False
        return daemon

    def _run_controller_thread(self, daemon, timeout=10):
        t = threading.Thread(target=daemon._run_controller)
        t.start()
        t.join(timeout=timeout)
        finished_in_time = not t.is_alive()
        if not finished_in_time:
            daemon.stop()  # don't leak a busy thread into other tests
            t.join(timeout=5)
        self.assertTrue(finished_in_time, "_run_controller must exit after stop()")


class TestBluezPluginGateNonRoot(_GateHarness):

    def test_disabled_nonroot_reports_hint_and_retries(self):
        """Plugin disabled + non-root → 'disconnected' with the hint, the
        daemon keeps retrying (no exception, no dead thread)."""
        daemon = self._make_daemon()
        fake_bluez = _FakeBluez(enabled=False)
        nxbtd._import_bluez = lambda: fake_bluez
        nxbtd._geteuid = lambda: 1000

        constructed = []
        daemon._make_controller = lambda: constructed.append(1)

        disconnects = []

        def on_status(state, detail):
            if state == "disconnected":
                disconnects.append(detail)
                if len(disconnects) >= 2:  # observed a retry
                    daemon.stop()

        daemon._current_session = _RecordingSession(on_status=on_status)
        self._run_controller_thread(daemon)

        self.assertGreaterEqual(len(disconnects), 2, "gate must retry")
        for detail in disconnects:
            self.assertEqual(detail, nxbtd.PLUGIN_DISABLED_HINT)
        self.assertGreaterEqual(fake_bluez.enabled_checks, 2)
        self.assertEqual(fake_bluez.toggle_calls, [], "non-root must not toggle")
        self.assertEqual(constructed, [], "controller must not be constructed")

    def test_recovers_when_plugin_enabled_externally(self):
        """Once the user runs 'sudo nuxbt toggle', the next retry proceeds —
        no daemon restart needed."""
        daemon = self._make_daemon()
        fake_bluez = _FakeBluez(enabled=False)
        nxbtd._import_bluez = lambda: fake_bluez
        nxbtd._geteuid = lambda: 1000

        class _StubCtrl:
            def connect(self, shutdown_event):
                pass

            def close(self):
                pass

        daemon._make_controller = _StubCtrl

        statuses = []

        def on_status(state, detail):
            statuses.append((state, detail))
            if state == "disconnected":
                fake_bluez.enabled = True  # user fixes it externally
            elif state == "connected":
                daemon.stop()

        daemon._current_session = _RecordingSession(on_status=on_status)
        self._run_controller_thread(daemon)

        self.assertIn(("disconnected", nxbtd.PLUGIN_DISABLED_HINT), statuses)
        self.assertIn(("connected", "Switch paired"), statuses)


class TestBluezPluginGateRoot(_GateHarness):

    def test_disabled_root_toggles_and_restarts_bluetooth(self):
        """Plugin disabled + root → toggle_clean_bluez(True) plus the two
        systemctl commands (subprocess is mocked — nothing really runs)."""
        daemon = self._make_daemon()
        fake_bluez = _FakeBluez(enabled=False)
        nxbtd._import_bluez = lambda: fake_bluez
        nxbtd._geteuid = lambda: 0

        run_calls = []

        def fake_run(cmd, check=False, capture_output=False, text=False):
            run_calls.append((tuple(cmd), check))
            return _FakeCompletedProcess()

        nxbtd.subprocess.run = fake_run

        class _StubCtrl:
            def connect(self, shutdown_event):
                pass

            def close(self):
                pass

        daemon._make_controller = _StubCtrl

        def on_status(state, detail):
            if state == "connected":
                daemon.stop()

        daemon._current_session = _RecordingSession(on_status=on_status)
        self._run_controller_thread(daemon)

        self.assertEqual(fake_bluez.toggle_calls, [True])
        self.assertEqual(run_calls, [
            (("systemctl", "daemon-reload"), True),
            (("systemctl", "restart", "bluetooth"), True),
        ])

    def test_systemctl_failure_is_disconnected_not_fatal(self):
        """A failing systemctl surfaces as 'disconnected' + retry."""
        import subprocess as _subprocess

        daemon = self._make_daemon()
        fake_bluez = _FakeBluez(enabled=False)
        # Re-arm: the gate short-circuits once enabled, so keep it disabled.
        fake_bluez.toggle_clean_bluez = lambda toggle: None
        nxbtd._import_bluez = lambda: fake_bluez
        nxbtd._geteuid = lambda: 0

        def fake_run(cmd, check=False, capture_output=False, text=False):
            raise _subprocess.CalledProcessError(1, cmd)

        nxbtd.subprocess.run = fake_run

        disconnects = []

        def on_status(state, detail):
            if state == "disconnected":
                disconnects.append(detail)
                daemon.stop()

        daemon._current_session = _RecordingSession(on_status=on_status)
        self._run_controller_thread(daemon)

        self.assertEqual(len(disconnects), 1)
        self.assertIn("systemctl", disconnects[0])


class TestBluezPluginGateSkipFlag(_GateHarness):

    def test_skip_flag_bypasses_gate(self):
        """--skip-bluez-check: nuxbt.bluez is never even imported."""
        daemon = self._make_daemon(skip_bluez_check=True)

        def must_not_import():
            raise AssertionError("gate must be bypassed with --skip-bluez-check")

        nxbtd._import_bluez = must_not_import

        class _StubCtrl:
            def connect(self, shutdown_event):
                pass

            def close(self):
                pass

        daemon._make_controller = _StubCtrl

        statuses = []

        def on_status(state, detail):
            statuses.append((state, detail))
            if state in ("connected", "disconnected"):
                daemon.stop()

        daemon._current_session = _RecordingSession(on_status=on_status)
        self._run_controller_thread(daemon)

        self.assertIn(("connected", "Switch paired"), statuses)

    def test_cli_flag_parses(self):
        parser = nxbtd._build_arg_parser()
        args = parser.parse_args(["--skip-bluez-check"])
        self.assertTrue(args.skip_bluez_check)
        args = parser.parse_args([])
        self.assertFalse(args.skip_bluez_check, "must default to False")


# ---------------------------------------------------------------------------
# Test: reconnect vs fresh-pair connect paths (nuxbt itself is faked)
# ---------------------------------------------------------------------------

class _FakeNx:
    """Stands in for a nuxbt.Nuxbt instance during connect()."""

    def __init__(self, initial_state="connected"):
        self.create_calls = []
        self.inputs = []
        self.state = {0: {"state": initial_state, "errors": None}}

    def create_controller(self, controller_type, reconnect_address=None):
        self.create_calls.append(
            dict(controller_type=controller_type, reconnect_address=reconnect_address)
        )
        return 0

    def create_input_packet(self):
        return _make_packet()

    def set_controller_input(self, index, pkt):
        self.inputs.append((index, pkt))


class TestConnectPaths(unittest.TestCase):

    def setUp(self):
        self._saved_import = nxbtd._import_bluez
        self._saved_press = nxbtd.PAIR_PRESS_SECONDS
        self._saved_release = nxbtd.PAIR_RELEASE_SECONDS
        self._had_nuxbt = hasattr(nxbtd, "nuxbt")
        self._saved_nuxbt = getattr(nxbtd, "nuxbt", None)
        # connect() references the module-level `nuxbt` name, absent in CI.
        import types
        nxbtd.nuxbt = types.SimpleNamespace(PRO_CONTROLLER="PRO_CONTROLLER")
        nxbtd.PAIR_PRESS_SECONDS = 0
        nxbtd.PAIR_RELEASE_SECONDS = 0

    def tearDown(self):
        nxbtd._import_bluez = self._saved_import
        nxbtd.PAIR_PRESS_SECONDS = self._saved_press
        nxbtd.PAIR_RELEASE_SECONDS = self._saved_release
        if self._had_nuxbt:
            nxbtd.nuxbt = self._saved_nuxbt
        else:
            del nxbtd.nuxbt

    def _make_controller(self, nx):
        ctrl = nxbtd.NxbtController.__new__(nxbtd.NxbtController)
        ctrl._nx = nx
        ctrl._index = None
        return ctrl

    def test_reconnect_passes_address_and_skips_pairing_sequence(self):
        """Previously-paired Switch found → reconnect_address is passed and
        the L+R/A sequence is NOT sent (the Switch is not on the menu)."""
        fake_bluez = _FakeBluez()
        fake_bluez.find_devices_by_alias = lambda alias: ["DC:68:EB:00:00:01"]
        nxbtd._import_bluez = lambda: fake_bluez

        nx = _FakeNx()
        ctrl = self._make_controller(nx)
        ctrl.connect(threading.Event())

        self.assertEqual(len(nx.create_calls), 1)
        self.assertEqual(nx.create_calls[0]["reconnect_address"], ["DC:68:EB:00:00:01"])
        self.assertEqual(nx.inputs, [], "no button presses on reconnect")

    def test_fresh_pair_sends_lr_then_a_sequence(self):
        """No previously-paired Switch → fresh pair, sequence IS sent."""
        fake_bluez = _FakeBluez()
        fake_bluez.find_devices_by_alias = lambda alias: []
        nxbtd._import_bluez = lambda: fake_bluez

        nx = _FakeNx()
        ctrl = self._make_controller(nx)
        ctrl.connect(threading.Event())

        self.assertEqual(nx.create_calls[0]["reconnect_address"], None)
        self.assertEqual(len(nx.inputs), 4, "press L+R, release, press A, release")
        press_lr, release_lr, press_a, release_a = [pkt for _, pkt in nx.inputs]
        self.assertTrue(press_lr["L"] and press_lr["R"])
        self.assertFalse(press_lr["A"])
        self.assertFalse(release_lr["L"] or release_lr["R"] or release_lr["A"])
        self.assertTrue(press_a["A"])
        self.assertFalse(press_a["L"] or press_a["R"])
        self.assertFalse(release_a["A"])

    def test_alias_lookup_failure_falls_back_to_fresh_pair(self):
        """A DBus error during the alias lookup must not abort the attempt."""
        def boom():
            raise RuntimeError("dbus unavailable")

        nxbtd._import_bluez = boom

        nx = _FakeNx()
        ctrl = self._make_controller(nx)
        ctrl.connect(threading.Event())

        self.assertEqual(nx.create_calls[0]["reconnect_address"], None)
        self.assertEqual(len(nx.inputs), 4, "fresh-pair sequence expected")

    def test_crashed_state_raises(self):
        """'crashed' controller state surfaces as an exception (existing
        behavior — feeds the supervision loop's disconnected+retry path)."""
        fake_bluez = _FakeBluez()
        fake_bluez.find_devices_by_alias = lambda alias: []
        nxbtd._import_bluez = lambda: fake_bluez

        nx = _FakeNx(initial_state="crashed")
        nx.state[0]["errors"] = "adapter gone"
        ctrl = self._make_controller(nx)
        with self.assertRaises(OSError) as cm:
            ctrl.connect(threading.Event())
        self.assertIn("adapter gone", str(cm.exception))

    def test_connect_wait_cancellable_by_shutdown_event(self):
        """A pre-set shutdown event cancels the wait without raising and
        without sending any buttons."""
        fake_bluez = _FakeBluez()
        fake_bluez.find_devices_by_alias = lambda alias: []
        nxbtd._import_bluez = lambda: fake_bluez

        nx = _FakeNx(initial_state="connecting")
        ctrl = self._make_controller(nx)
        ev = threading.Event()
        ev.set()
        ctrl.connect(ev)
        self.assertEqual(nx.inputs, [])


# ---------------------------------------------------------------------------
# Test: session routes inputs through the CURRENT controller, not a snapshot
# ---------------------------------------------------------------------------

class _LiveRoutingController:
    def __init__(self):
        self.inputs = []

    def apply_input(self, buttons, lx, ly, rx, ry):
        self.inputs.append((buttons, lx, ly, rx, ry))


class TestSessionLiveControllerRouting(unittest.TestCase):
    """Regression: the host's bridge reconnects to the socket before the
    Switch finishes pairing; a controller snapshot taken at accept time would
    pin the session to a stale fallback forever. The session must resolve the
    controller per input via the getter."""

    def _session_with_getter(self, getter):
        a, b = socket.socketpair()
        self.addCleanup(a.close)
        session = nxbtd.ClientSession(
            conn=b, controller=getter, status_event=threading.Event()
        )
        self.addCleanup(b.close)
        return session

    def test_input_before_controller_ready_is_dropped_then_routed(self):
        holder = {"ctrl": None}
        session = self._session_with_getter(lambda: holder["ctrl"])

        # Not ready: input is dropped, no crash.
        session._handle_line(b'{"type":"input","buttons":3}')

        # Controller appears (e.g. Switch finished pairing): same session
        # must start routing to it without reconnecting.
        ctrl = _LiveRoutingController()
        holder["ctrl"] = ctrl
        session._handle_line(b'{"type":"input","buttons":7,"lx":0.5}')

        self.assertEqual(len(ctrl.inputs), 1)
        self.assertEqual(ctrl.inputs[0][0], 7)
        self.assertAlmostEqual(ctrl.inputs[0][1], 0.5)

    def test_controller_swap_is_picked_up(self):
        first, second = _LiveRoutingController(), _LiveRoutingController()
        holder = {"ctrl": first}
        session = self._session_with_getter(lambda: holder["ctrl"])

        session._handle_line(b'{"type":"input","buttons":1}')
        holder["ctrl"] = second  # daemon recycled the controller
        session._handle_line(b'{"type":"input","buttons":2}')

        self.assertEqual([i[0] for i in first.inputs], [1])
        self.assertEqual([i[0] for i in second.inputs], [2])

    def test_plain_controller_argument_still_works(self):
        ctrl = _LiveRoutingController()
        a, b = socket.socketpair()
        self.addCleanup(a.close)
        self.addCleanup(b.close)
        session = nxbtd.ClientSession(
            conn=b, controller=ctrl, status_event=threading.Event()
        )
        session._handle_line(b'{"type":"input","buttons":9}')
        self.assertEqual([i[0] for i in ctrl.inputs], [9])

    def test_daemon_get_controller_reflects_current(self):
        daemon = nxbtd.Daemon(socket_path="/tmp/unused-live.sock", mock=True)
        self.assertIsNone(daemon._get_controller())
        ctrl = _LiveRoutingController()
        with daemon._session_lock:
            daemon._controller = ctrl
        self.assertIs(daemon._get_controller(), ctrl)


# ---------------------------------------------------------------------------
# Test: socket permissions (root daemon, non-root playgate-host client)
# ---------------------------------------------------------------------------

@unittest.skipUnless(hasattr(socket, "AF_UNIX"), "AF_UNIX socket permissions are Unix-only")
class TestSocketPermissions(unittest.TestCase):
    """Connecting to a UNIX socket needs WRITE permission on its inode, so
    the daemon must chmod (and optionally chgrp) the socket after bind."""

    def _run_daemon(self, **kwargs):
        """Start Daemon.run in a thread, wait until it accepts connections.

        Waits for a successful connect, not mere existence of the socket
        file: the file appears at bind(), but permissions are applied
        between bind() and listen(), so connectability guarantees
        _apply_socket_permissions has completed.
        """
        path = os.path.join(tempfile.mkdtemp(), "nxbtd.sock")
        daemon = nxbtd.Daemon(socket_path=path, mock=True, **kwargs)
        t = threading.Thread(target=daemon.run, daemon=True)
        t.start()
        deadline = time.monotonic() + 5
        while True:
            self.assertLess(time.monotonic(), deadline, "daemon never listened")
            try:
                probe = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
                probe.connect(path)
                probe.close()
                break
            except OSError:
                time.sleep(0.01)
        self.addCleanup(t.join, timeout=5)
        self.addCleanup(daemon.stop)
        return daemon, path

    def test_default_mode_is_group_writable(self):
        _, path = self._run_daemon()
        mode = os.stat(path).st_mode & 0o777
        self.assertEqual(mode, 0o660)

    def test_custom_mode_applied(self):
        _, path = self._run_daemon(socket_mode=0o600)
        mode = os.stat(path).st_mode & 0o777
        self.assertEqual(mode, 0o600)

    def test_socket_group_applied(self):
        import grp
        # Pick a group the test user belongs to, so chgrp is permitted
        # without root. Prefer a supplementary group over the primary one
        # so the chown is observable; fall back to the primary group.
        gids = os.getgroups() or [os.getgid()]
        gid = next((g for g in gids if g != os.getgid()), gids[0])
        group_name = grp.getgrgid(gid).gr_name

        _, path = self._run_daemon(socket_group=group_name)
        self.assertEqual(os.stat(path).st_gid, gid)
        self.assertEqual(os.stat(path).st_mode & 0o777, 0o660)

    def test_unknown_group_warns_but_serves(self):
        with self.assertLogs("nxbtd", level="WARNING") as captured:
            _, path = self._run_daemon(socket_group="no-such-group-xyzzy")
        self.assertTrue(any("could not set socket group" in line
                            for line in captured.output))
        # Daemon still serving: socket exists with the requested mode.
        self.assertEqual(os.stat(path).st_mode & 0o777, 0o660)

    def test_cli_flags_parse(self):
        args = nxbtd._build_arg_parser().parse_args(
            ["--socket-group", "playgate", "--socket-mode", "640"]
        )
        self.assertEqual(args.socket_group, "playgate")
        self.assertEqual(args.socket_mode, 0o640)


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

if __name__ == '__main__':
    unittest.main(verbosity=2)
