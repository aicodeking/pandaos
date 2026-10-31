#!/usr/bin/env python3
"""panda-cu AT-SPI helper: one long-lived process per `panda-cu mcp`.

Requests and replies are JSON lines on stdin and stdout; every reply echoes
the request's "seq":

  {"cmd": "tree", "app": A, "max_depth": 30, "limit": 2000, "interactive": false, "active": false}
  {"cmd": "find", "app": A, "role": R, "text": T, "limit": 50}
  {"cmd": "get", "id": "e12"}                 the element now, plus its window
  {"cmd": "act", "id": "e12", "action": "click|press|focus|set_text", "text": T, "role": R, "name": N}

Element ids ("e12") name one accessible object for the life of this
process, whichever request returned it, so a later cu_tree or cu_wait never
invalidates an id from an earlier one. An id whose object is gone answers
{"error": "... gone ..."}. Every element carries "win", the id of its
toplevel window; replies carry "windows": {id: window element}.

find uses the AT-SPI Collection API (one GetMatches call per window) where
the toolkit has it (GTK 3, Firefox, Chromium via ATK), else a tree walk.
The libatspi cache is off: each answer is read from the app, never stale.
"""
import json
import os
import sys

import gi

gi.require_version("Atspi", "2.0")
from gi.repository import Atspi, GLib  # noqa: E402

S = Atspi.StateType
INTERACTIVE = {
    "push button", "toggle button", "check box", "radio button", "menu item", "check menu item",
    "radio menu item", "menu", "combo box", "entry", "text", "password text", "link", "page tab",
    "list item", "tree item", "spin button", "slider", "table cell", "icon", "toggle",
}
STATES = (("focused", S.FOCUSED), ("editable", S.EDITABLE), ("checked", S.CHECKED),
          ("selected", S.SELECTED), ("expanded", S.EXPANDED), ("active", S.ACTIVE))
PREFERRED = {"click": ("click", "press", "activate", "toggle", "jump", "open"),
             "press": ("press", "click", "activate", "toggle")}
# Newer at-spi2-core names GtkButton's role "button"; report the classic name.
ROLE_ALIASES = {"button": "push button"}
CONTAINERS = ("frame", "application", "window", "panel", "filler", "dialog", "file chooser")


def _roles():
    m = {}
    for v in range(int(Atspi.Role.LAST_DEFINED)):
        try:
            r = Atspi.Role(v)
            m[ROLE_ALIASES.get(Atspi.role_get_name(r), Atspi.role_get_name(r))] = r
        except (ValueError, TypeError):
            pass
    m["push button"] = Atspi.Role.PUSH_BUTTON
    return m


ROLE_BY_NAME = _roles()


class Ids:
    """Element ids for accessible objects. libatspi hands out one GObject per
    remote object, and pygobject one wrapper per GObject, so the wrapper is
    the key. ponytail: forgets everything past 100k objects (old ids then
    answer "unknown"); age out entries if that ever bites."""

    def __init__(self):
        self.obj, self.id, self.n = {}, {}, 0

    def of(self, acc):
        i = self.id.get(acc)
        if i is None:
            if self.n >= 100_000:
                self.obj.clear()
                self.id.clear()
            self.n += 1
            i = f"e{self.n}"
            self.obj[i], self.id[acc] = acc, i
        return i

    def get(self, i):
        acc = self.obj.get(i)
        if acc is None:
            raise LookupError(f"unknown element_id {i!r}: take a cu_tree, cu_find or cu_screenshot{{marks}} first")
        return acc


IDS = Ids()


def role_of(acc):
    r = acc.get_role_name()
    return ROLE_ALIASES.get(r, r)


def describe(acc, win=None, depth=None):
    """One element as JSON, plus whether it is on screen (showing+visible)."""
    role, name = role_of(acc), acc.get_name() or ""
    ss = acc.get_state_set()
    if ss.contains(S.DEFUNCT):
        raise LookupError("element is gone (its window or app closed)")
    e = {"id": IDS.of(acc), "role": role, "name": name[:200]}
    if depth is not None:
        e["d"] = depth
    if win is not None:
        e["win"] = win
    st = [n for n, s in STATES if ss.contains(s)]
    if not ss.contains(S.ENABLED):
        st.append("disabled")
    if st:
        e["states"] = st
    comp = acc.get_component_iface()
    if comp is not None:
        r = comp.get_extents(Atspi.CoordType.SCREEN)
        e["bbox"] = [r.x, r.y, r.width, r.height]
    act = acc.get_action_iface()
    if act is not None:
        names = [act.get_action_name(i) for i in range(act.get_n_actions())]
        if names:
            e["actions"] = names
    txt = acc.get_text_iface()
    if txt is not None and role != "label":
        n = txt.get_character_count()
        if n:
            # Atspi.Text.get_text, not txt.get_text: the iface is the
            # Accessible itself, whose own get_text() shadows it.
            e["text"] = Atspi.Text.get_text(txt, 0, min(n, 500))
    return e, ss.contains(S.SHOWING) and ss.contains(S.VISIBLE)


def interactive(e):
    b = e.get("bbox")
    if not b or b[2] <= 0 or b[3] <= 0 or "disabled" in e.get("states", ()):
        return False
    return e["role"] in INTERACTIVE or "editable" in e.get("states", ()) or (
        "actions" in e and e["role"] not in CONTAINERS)


def children(acc):
    try:
        n = acc.get_child_count()
    except GLib.Error:
        return
    for i in range(n):
        try:
            ch = acc.get_child_at_index(i)
        except GLib.Error:
            continue
        if ch is not None:
            yield ch


def apps(want):
    """(app, [toplevel]) pairs, filtered by app name or window title."""
    want = (want or "").lower()
    for app in children(Atspi.get_desktop(0)):
        try:
            name = (app.get_name() or "").lower()
            tops = list(children(app))
            titles = [(t.get_name() or "").lower() for t in tops]
        except GLib.Error:
            continue
        if not want or want in name or any(want in t for t in titles):
            yield app, tops


class Out:
    """Collects elements and the windows they belong to."""

    def __init__(self, limit):
        self.elements, self.windows, self.limit = [], {}, limit

    def full(self):
        return len(self.elements) >= self.limit

    def window(self, top):
        e, showing = describe(top, depth=1)
        self.windows[e["id"]] = e
        return e, showing


def walk(acc, win, depth, out, a, keep):
    if out.full():
        return
    try:
        e, showing = describe(acc, win, depth)
    except (GLib.Error, LookupError):
        return
    if not showing:
        return  # hidden subtree
    if keep(e):
        out.elements.append(e)
    if depth < a.get("max_depth", 30):
        for ch in children(acc):
            walk(ch, win, depth + 1, out, a, keep)


def toplevels(a, out):
    for app, tops in apps(a.get("app")):
        try:
            ae, _ = describe(app, depth=0)
        except (GLib.Error, LookupError):
            continue
        for top in tops:
            try:
                we, showing = out.window(top)
            except (GLib.Error, LookupError):
                continue
            if not showing or (a.get("active") and "active" not in we.get("states", ())):
                continue
            yield ae, top, we


def tree(a):
    out = Out(a.get("limit", 2000))
    keep = interactive if a.get("interactive") else (lambda e: True)
    seen = set()
    for ae, top, we in toplevels(a, out):
        if ae["id"] not in seen and not a.get("interactive"):
            seen.add(ae["id"])
            out.elements.append(ae)
        if keep(we):
            out.elements.append(dict(we, win=we["id"]))
        for ch in children(top):
            walk(ch, we["id"], 2, out, a, keep)
    return {"elements": out.elements, "windows": out.windows, "truncated": out.full()}


def collection_matches(top, roles):
    col = top.get_collection_iface()
    if col is None:
        return None
    all_, any_ = Atspi.CollectionMatchType.ALL, Atspi.CollectionMatchType.ANY
    rule = Atspi.MatchRule.new(Atspi.StateSet.new([S.SHOWING]), all_, {}, all_,
                               roles, any_ if roles else all_, [], all_, False)
    try:
        return col.get_matches(rule, Atspi.CollectionSortOrder.CANONICAL, 0, True)
    except (GLib.Error, TypeError):
        return None


def find(a):
    role = ROLE_ALIASES.get((a.get("role") or "").lower(), (a.get("role") or "").lower())
    text = (a.get("text") or "").lower()
    if role and role not in ROLE_BY_NAME:
        raise ValueError(f"unknown role {a.get('role')!r}")
    out = Out(a.get("limit", 50))

    def keep(e):
        return (not role or e["role"] == role) and (
            not text or text in e["name"].lower() or text in e.get("text", "").lower())

    via = set()
    for _, top, we in toplevels(a, out):
        if keep(we):
            out.elements.append(dict(we, win=we["id"]))
        found = collection_matches(top, [ROLE_BY_NAME[role]] if role else [])
        if found is None:  # no Collection (e.g. GTK 4): walk
            via.add("walk")
            for ch in children(top):
                walk(ch, we["id"], 2, out, {"max_depth": 30}, keep)
            continue
        via.add("collection")
        for acc in found:
            if out.full():
                break
            try:
                if text and text not in (acc.get_name() or "").lower() and not has_text(acc, text):
                    continue
                e, showing = describe(acc, we["id"])
            except (GLib.Error, LookupError):
                continue
            if showing and keep(e):
                out.elements.append(e)
    return {"elements": out.elements, "windows": out.windows, "via": sorted(via)}


def has_text(acc, text):
    t = acc.get_text_iface()
    if t is None:
        return False
    n = t.get_character_count()
    return n > 0 and text in Atspi.Text.get_text(t, 0, min(n, 500)).lower()


def window_of(acc):
    """The toplevel (child of the application) that holds acc."""
    cur = acc
    for _ in range(64):
        parent = cur.get_parent()
        if parent is None:
            break
        if parent.get_role() == Atspi.Role.APPLICATION:
            return cur
        cur = parent
    raise LookupError("element is not inside a window")


def get(a):
    acc = IDS.get(a["id"])
    try:
        e, showing = describe(acc)
        top = window_of(acc)
        we, wshow = describe(top, depth=1)
    except GLib.Error as err:
        raise LookupError(f"element {a['id']} is gone: {err.message}") from err
    e["win"] = we["id"]
    return {"element": e, "showing": showing and wshow, "windows": {we["id"]: we}}


def act(a):
    acc = IDS.get(a["id"])
    try:
        role, name = role_of(acc), (acc.get_name() or "")[:200]
        if acc.get_state_set().contains(S.DEFUNCT):
            raise LookupError(f"element {a['id']} is gone")
    except GLib.Error as err:
        raise LookupError(f"element {a['id']} is gone: {err.message}") from err
    if a.get("role") is not None and (role != a["role"] or name != a.get("name", "")):
        raise LookupError(f"element {a['id']} changed (now {role} {name!r}); call cu_find again")
    action = a["action"]
    if action == "focus":
        comp = acc.get_component_iface()
        if comp is None or not comp.grab_focus():
            raise RuntimeError("element cannot take focus")
        return {"ok": True, "via": "grab_focus"}
    if action == "set_text":
        et = acc.get_editable_text_iface()
        if et is None or not et.set_text_contents(a.get("text") or ""):
            raise RuntimeError("element has no editable text; focus it and use cu_type")
        return {"ok": True, "via": "set_text_contents"}
    ai = acc.get_action_iface()
    names = [ai.get_action_name(i) for i in range(ai.get_n_actions())] if ai is not None else []
    for want in PREFERRED.get(action, ()):
        if want in names:
            ai.do_action(names.index(want))
            return {"ok": True, "via": "action:" + want}
    if names:
        ai.do_action(0)
        return {"ok": True, "via": "action:" + names[0]}
    raise RuntimeError("element has no AT-SPI action; use cu_click with its element_id (pixels)")


CMDS = {"tree": tree, "find": find, "get": get, "act": act, "ping": lambda a: {"ok": True}}


def handle(line):
    req = {}
    try:
        req = json.loads(line)
        res = CMDS[req["cmd"]](req)
    except (LookupError, RuntimeError, GLib.Error, ValueError, TypeError, KeyError) as e:
        res = {"error": getattr(e, "message", None) or str(e)}
    res["seq"] = req.get("seq")
    sys.stdout.write(json.dumps(res, ensure_ascii=False) + "\n")
    sys.stdout.flush()


def main():
    Atspi.init()
    Atspi.set_timeout(2000, 5000)
    try:
        Atspi.get_desktop(0).set_cache_mask(Atspi.Cache.NONE)
    except (AttributeError, GLib.Error):
        pass
    loop, buf = GLib.MainLoop(), [b""]

    # A GLib main loop, not a blocking read: libatspi's D-Bus connection is
    # dispatched from it between requests, so its queue never piles up.
    def on_stdin(fd, cond):
        chunk = os.read(fd, 1 << 16)
        if not chunk:
            loop.quit()
            return False
        buf[0] += chunk
        while b"\n" in buf[0]:
            line, buf[0] = buf[0].split(b"\n", 1)
            if line.strip():
                handle(line)
        return True

    GLib.io_add_watch(sys.stdin.fileno(), GLib.PRIORITY_DEFAULT, GLib.IO_IN | GLib.IO_HUP, on_stdin)
    loop.run()


if __name__ == "__main__":
    main()
