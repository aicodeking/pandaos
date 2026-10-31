#!/usr/bin/env python3
"""panda-cu AT-SPI helper. Called by `panda-cu mcp` by argv; prints JSON.

  panda-cu-atspi tree [--max-depth N] [--app NAME] [--active] [--limit N]
  panda-cu-atspi act ID ACTION [--text T] [--role R] [--name N]

Element ids are child-index paths from the desktop ("2.0.5.1"): stable for
as long as the tree does not change. `act` re-resolves the path and refuses
if the role or name no longer match what the caller saw.
"""
import argparse
import json
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


def describe(acc, path, depth):
    role, name = acc.get_role_name(), acc.get_name() or ""
    ss = acc.get_state_set()
    e = {"id": path, "d": depth, "role": role, "name": name[:200]}
    showing = ss.contains(S.SHOWING) and ss.contains(S.VISIBLE)
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
            e["text"] = txt.get_text(0, min(n, 500))
    return e, showing


def interactive(e):
    b = e.get("bbox")
    if not b or b[2] <= 0 or b[3] <= 0 or "disabled" in e.get("states", ()):
        return False
    return e["role"] in INTERACTIVE or "editable" in e.get("states", ()) or (
        "actions" in e and e["role"] not in ("frame", "application", "window", "panel", "filler"))


def walk(acc, path, depth, out, a):
    if len(out) >= a.limit:
        return
    try:
        e, showing = describe(acc, path, depth)
    except GLib.Error:
        return  # gone or not answering
    if depth >= 1 and not showing:
        return  # hidden subtree
    if depth == 1 and a.active and "active" not in e.get("states", ()):
        return  # not the focused window
    if not a.interactive or interactive(e):
        out.append(e)
    if depth >= a.max_depth:
        return
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
            walk(ch, f"{path}.{i}", depth + 1, out, a)


def tree(a):
    desk, out = Atspi.get_desktop(0), []
    want = (a.app or "").lower()
    for i in range(desk.get_child_count()):
        app = desk.get_child_at_index(i)
        if app is None:
            continue
        try:
            name = (app.get_name() or "").lower()
            frames = [app.get_child_at_index(j) for j in range(min(app.get_child_count(), 8))]
            titles = [(f.get_name() or "").lower() for f in frames if f is not None]
        except GLib.Error:
            continue
        if want and want not in name and not any(want in t for t in titles):
            continue
        walk(app, str(i), 0, out, a)
    return {"elements": out, "truncated": len(out) >= a.limit}


def resolve(path):
    acc = Atspi.get_desktop(0)
    for p in path.split("."):
        acc = acc.get_child_at_index(int(p)) if acc is not None else None
    if acc is None:
        raise LookupError(f"element {path} is gone; call cu_tree again")
    return acc


def act(a):
    acc = resolve(a.id)
    if a.role is not None and (acc.get_role_name() != a.role or (acc.get_name() or "")[:200] != a.name):
        raise LookupError(f"element {a.id} changed (now {acc.get_role_name()} {acc.get_name()!r}); call cu_tree again")
    if a.action == "focus":
        comp = acc.get_component_iface()
        if comp is None or not comp.grab_focus():
            raise RuntimeError("element cannot take focus")
        return {"ok": True, "via": "grab_focus"}
    if a.action == "set_text":
        et = acc.get_editable_text_iface()
        if et is None or not et.set_text_contents(a.text or ""):
            raise RuntimeError("element has no editable text; focus it and use cu_type")
        return {"ok": True, "via": "set_text_contents"}
    ai = acc.get_action_iface()
    names = [ai.get_action_name(i) for i in range(ai.get_n_actions())] if ai is not None else []
    for want in PREFERRED[a.action]:
        if want in names:
            ai.do_action(names.index(want))
            return {"ok": True, "via": "action:" + want}
    if names:
        ai.do_action(0)
        return {"ok": True, "via": "action:" + names[0]}
    raise RuntimeError("element has no AT-SPI action; use cu_click with its element_id (pixels)")


def main():
    p = argparse.ArgumentParser(prog="panda-cu-atspi")
    sub = p.add_subparsers(dest="cmd", required=True)
    t = sub.add_parser("tree")
    t.add_argument("--max-depth", type=int, default=30)
    t.add_argument("--app")
    t.add_argument("--active", action="store_true")
    t.add_argument("--interactive", action="store_true")
    t.add_argument("--limit", type=int, default=2000)
    c = sub.add_parser("act")
    c.add_argument("id")
    c.add_argument("action", choices=["click", "press", "focus", "set_text"])
    c.add_argument("--text")
    c.add_argument("--role")
    c.add_argument("--name", default="")
    a = p.parse_args()
    Atspi.set_timeout(2000, 5000)
    try:
        res = tree(a) if a.cmd == "tree" else act(a)
    except (LookupError, RuntimeError, GLib.Error, ValueError) as e:
        json.dump({"error": str(e)}, sys.stdout)
        return 1
    json.dump(res, sys.stdout, ensure_ascii=False)
    return 0


if __name__ == "__main__":
    sys.exit(main())
