"""Drive a VM from outside through QEMU's QMP socket: screenshots, mouse, keys.

The generic tool set is built only on this, so it is identical for every guest
OS. Pointer events go to a usb-tablet (absolute axes 0..32767); keys go to the
default keyboard as QMP qcodes, assuming a US layout in the guest.
"""
import json
import os
import socket
import struct
import threading
import time
import zlib

ABS_MAX = 32767

# Printable ASCII -> (qcode, shift). Letters and digits are filled in below.
_UNSHIFTED = {
    "-": "minus", "=": "equal", "[": "bracket_left", "]": "bracket_right",
    "\\": "backslash", ";": "semicolon", "'": "apostrophe", "`": "grave_accent",
    ",": "comma", ".": "dot", "/": "slash", " ": "spc", "\n": "ret", "\t": "tab",
}
_SHIFTED = {
    "!": "1", "@": "2", "#": "3", "$": "4", "%": "5", "^": "6", "&": "7",
    "*": "8", "(": "9", ")": "0", "_": "minus", "+": "equal", "{": "bracket_left",
    "}": "bracket_right", "|": "backslash", ":": "semicolon", '"': "apostrophe",
    "~": "grave_accent", "<": "comma", ">": "dot", "?": "slash",
}
CHARS = {c: (q, False) for c, q in _UNSHIFTED.items()}
CHARS.update({c: (q, True) for c, q in _SHIFTED.items()})
for _c in "abcdefghijklmnopqrstuvwxyz":
    CHARS[_c] = (_c, False)
    CHARS[_c.upper()] = (_c, True)
for _c in "0123456789":
    CHARS[_c] = (_c, False)

# Key names accepted in key("ctrl+shift+n") combos, besides single characters.
NAMES = {
    "ctrl": "ctrl", "control": "ctrl", "shift": "shift", "alt": "alt",
    "altgr": "alt_r", "super": "meta_l", "meta": "meta_l", "win": "meta_l",
    "cmd": "meta_l", "enter": "ret", "return": "ret", "esc": "esc",
    "escape": "esc", "tab": "tab", "backspace": "backspace", "delete": "delete",
    "del": "delete", "space": "spc", "up": "up", "down": "down", "left": "left",
    "right": "right", "home": "home", "end": "end", "pageup": "pgup",
    "page_up": "pgup", "pgup": "pgup", "pagedown": "pgdn", "page_down": "pgdn",
    "pgdn": "pgdn", "insert": "insert", "menu": "menu", "plus": "equal",
    "minus": "minus", "capslock": "caps_lock", "print": "print",
}
NAMES.update({f"f{i}": f"f{i}" for i in range(1, 13)})


def char_keys(ch):
    """qcodes to press together for one character, e.g. 'A' -> ['shift', 'a']."""
    if ch not in CHARS:
        raise ValueError(f"cannot type {ch!r}: only printable ASCII, newline and tab")
    q, shift = CHARS[ch]
    return ["shift", q] if shift else [q]


def combo_keys(combo):
    """'ctrl+shift+n' / 'Return' / 'ctrl+S' -> qcodes, modifiers first."""
    parts = combo.split("+")
    if combo.endswith("+"):  # "ctrl++" / "+" mean the '+' key
        parts = [p for p in parts if p] + ["+"]
    keys = []
    for p in parts:
        p = p.strip()
        if not p:
            raise ValueError(f"bad key combo {combo!r}")
        if p.lower() in NAMES:
            keys.append(NAMES[p.lower()])
        elif len(p) == 1:
            # A letter inside a combo names the key; shift only if explicitly given.
            ks = char_keys(p.lower() if p.isalpha() else p)
            keys.extend(k for k in ks if k not in keys)
        else:
            raise ValueError(f"unknown key {p!r} in {combo!r}")
    return keys


def png_size(path):
    with open(path, "rb") as f:
        head = f.read(24)
    if head[:8] != b"\x89PNG\r\n\x1a\n":
        raise ValueError(f"{path} is not a PNG")
    return struct.unpack(">II", head[16:24])


class QMP:
    def __init__(self, path, timeout=30):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(timeout)
        self.sock.connect(path)
        self.f = self.sock.makefile("rwb")
        self._read()  # greeting
        self.execute("qmp_capabilities")
        self.size = (1280, 800)

    def _read(self):
        while True:
            line = self.f.readline()
            if not line:
                raise ConnectionError("QMP socket closed")
            msg = json.loads(line)
            if "event" not in msg:
                return msg

    def execute(self, cmd, args=None):
        req = {"execute": cmd}
        if args is not None:
            req["arguments"] = args
        self.f.write(json.dumps(req).encode() + b"\n")
        self.f.flush()
        msg = self._read()
        if "error" in msg:
            raise RuntimeError(f"QMP {cmd}: {msg['error'].get('desc', msg['error'])}")
        return msg.get("return")

    def hmp(self, command):
        """A human-monitor command (savevm/loadvm); QEMU reports their errors as text."""
        out = self.execute("human-monitor-command", {"command-line": command})
        if isinstance(out, str) and out.strip():
            raise RuntimeError(f"{command}: {out.strip()}")

    def close(self):
        try:
            self.sock.close()
        except OSError:
            pass

    # -- screen -------------------------------------------------------------
    def screendump(self, path):
        self.execute("screendump", {"filename": os.path.abspath(path), "format": "png"})
        self.size = png_size(path)
        return self.size

    # -- pointer ------------------------------------------------------------
    def _abs(self, x, y):
        w, h = self.size
        if not (0 <= x < w and 0 <= y < h):
            raise ValueError(f"({x}, {y}) is outside the {w}x{h} screen")
        return [{"type": "abs", "data": {"axis": "x", "value": round(x * ABS_MAX / max(w - 1, 1))}},
                {"type": "abs", "data": {"axis": "y", "value": round(y * ABS_MAX / max(h - 1, 1))}}]

    def _events(self, events):
        self.execute("input-send-event", {"events": events})

    def move(self, x, y):
        self._events(self._abs(x, y))

    def _button(self, button, down):
        self._events([{"type": "btn", "data": {"down": down, "button": button}}])

    def click(self, x, y, button="left", count=1):
        if button not in ("left", "right", "middle"):
            raise ValueError(f"bad button {button!r}")
        self.move(x, y)
        time.sleep(0.05)
        for i in range(count):
            if i:
                time.sleep(0.08)
            self._button(button, True)
            time.sleep(0.03)
            self._button(button, False)

    def drag(self, x1, y1, x2, y2, steps=12):
        self.move(x1, y1)
        time.sleep(0.1)
        self._button("left", True)
        time.sleep(0.15)
        for i in range(1, steps + 1):
            self.move(round(x1 + (x2 - x1) * i / steps), round(y1 + (y2 - y1) * i / steps))
            time.sleep(0.04)
        time.sleep(0.15)
        self._button("left", False)

    def scroll(self, x, y, direction="down", amount=3):
        if direction not in ("up", "down"):
            raise ValueError("direction must be up or down")
        self.move(x, y)
        btn = "wheel-up" if direction == "up" else "wheel-down"
        for _ in range(max(1, min(int(amount), 50))):
            self._button(btn, True)
            self._button(btn, False)
            time.sleep(0.03)

    # -- keyboard -----------------------------------------------------------
    def send_keys(self, qcodes, hold_ms=30):
        """Press qcodes in order, hold, release in reverse. Not QMP send-key: that
        queues releases on QEMU's virtual-clock timers, and after a loadvm (the
        clock goes back) the queue stops draining and every later key is lost."""
        ev = lambda q, down: {"type": "key", "data": {"down": down, "key": {"type": "qcode", "data": q}}}  # noqa: E731
        self._events([ev(q, True) for q in qcodes])
        time.sleep(hold_ms / 1000)
        self._events([ev(q, False) for q in reversed(qcodes)])

    def key(self, combo):
        self.send_keys(combo_keys(combo), hold_ms=60)
        time.sleep(0.08)

    def type(self, text):
        keys = [char_keys(c) for c in text]  # validate everything before typing
        for ks in keys:
            self.send_keys(ks)
            time.sleep(0.025)


def write_png(path, w, h, rgb=(32, 32, 40)):
    """A solid-colour PNG (stdlib only), for the fake QMP server."""
    row = b"\x00" + bytes(rgb) * w
    raw = zlib.compress(row * h, 6)

    def chunk(t, d):
        return struct.pack(">I", len(d)) + t + d + struct.pack(">I", zlib.crc32(t + d) & 0xFFFFFFFF)
    with open(path, "wb") as f:
        f.write(b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0))
                + chunk(b"IDAT", raw) + chunk(b"IEND", b""))


class FakeQMPServer:
    """A QMP-speaking unix socket that records commands (tests and --dry-run)."""

    def __init__(self, path, size=(1280, 800)):
        self.path, self.size, self.commands = path, size, []
        self.srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.srv.bind(path)
        self.srv.listen(4)
        self.n = 0
        threading.Thread(target=self._serve, daemon=True).start()

    def _serve(self):
        while True:
            try:
                conn, _ = self.srv.accept()
            except OSError:
                return
            threading.Thread(target=self._client, args=(conn,), daemon=True).start()

    def _client(self, conn):
        f = conn.makefile("rwb")
        f.write(b'{"QMP": {"version": {"qemu": {"major": 8, "minor": 2, "micro": 0}}, "capabilities": []}}\n')
        f.flush()
        for line in f:
            req = json.loads(line)
            self.commands.append(req)
            cmd, args = req.get("execute"), req.get("arguments", {})
            if cmd == "screendump":
                # Alternate colours so "did the screen change" sees a change.
                self.n += 1
                write_png(args["filename"], *self.size, rgb=(self.n % 2 * 50, 30, 40))
            f.write(b'{"event": "NOISE", "data": {}}\n')
            f.write(b'{"return": {}}\n')
            f.flush()

    def close(self):
        self.srv.close()
