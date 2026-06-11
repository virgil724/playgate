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
import socket
import sys
import threading
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
# Main
# ---------------------------------------------------------------------------

if __name__ == '__main__':
    unittest.main(verbosity=2)
