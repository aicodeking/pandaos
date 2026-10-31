#!/usr/bin/env python3
"""vmtest app "CU Lab" (GTK 3): small targets, a menu, a slider, a long
list, a multi-line text view and two dialogs. Every event appends one line
to ~/cu-lab.log, so the test knows what really happened:

  small:s3   menu:hello   scale:73   row:59   dialog:confirm   message:ok
"""
import os

import gi

gi.require_version("Gtk", "3.0")
from gi.repository import GLib, Gtk  # noqa: E402

GLib.set_prgname("cu-lab")
log = open(os.path.expanduser("~/cu-lab.log"), "a", buffering=1)

css = Gtk.CssProvider()
css.load_from_data(b"button.tiny { min-height: 0; min-width: 0; padding: 0; margin: 0; }")
Gtk.StyleContext.add_provider_for_screen(
    Gtk.Window().get_screen(), css, Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)

win = Gtk.Window(title="CU Lab")
box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=8)

# Menu bar: Actions > Say hello / Open dialog
bar, menu = Gtk.MenuBar(), Gtk.Menu()
top = Gtk.MenuItem(label="Actions")
top.set_submenu(menu)
for label, fn in (("Say hello", lambda *_: log.write("menu:hello\n")),
                  ("Open dialog", lambda *_: open_dialog())):
    item = Gtk.MenuItem(label=label)
    item.connect("activate", fn)
    menu.append(item)
bar.append(top)
box.pack_start(bar, False, False, 0)

# 12 buttons of 24x24 px, named s0..s11 for AT-SPI (no visible label).
row = Gtk.Box(spacing=6, halign=Gtk.Align.START, margin_start=12)
for i in range(12):
    b = Gtk.Button()
    b.get_style_context().add_class("tiny")
    b.set_size_request(24, 24)
    b.get_accessible().set_name(f"s{i}")
    b.connect("clicked", lambda b, i=i: log.write(f"small:s{i}\n"))
    row.pack_start(b, False, False, 0)
box.pack_start(row, False, False, 0)

# A slider 0..100 (drag target); logged on release.
scale = Gtk.Scale.new_with_range(Gtk.Orientation.HORIZONTAL, 0, 100, 1)
scale.set_size_request(400, -1)
scale.set_halign(Gtk.Align.START)
scale.set_draw_value(True)
scale.get_accessible().set_name("Level")
scale.connect("button-release-event", lambda s, e: log.write(f"scale:{int(s.get_value())}\n") and False)
box.pack_start(scale, False, False, 0)

hb = Gtk.Box(spacing=12)
# A list of 60 buttons in a 200 px scrolled window: row59 needs scrolling.
sw = Gtk.ScrolledWindow()
sw.set_size_request(300, 200)
lst = Gtk.Box(orientation=Gtk.Orientation.VERTICAL)
for i in range(60):
    b = Gtk.Button(label=f"row{i}")
    b.connect("clicked", lambda b, i=i: log.write(f"row:{i}\n"))
    lst.pack_start(b, False, False, 0)
sw.add(lst)
hb.pack_start(sw, False, False, 0)

# Multi-line text for cu_type.
tv = Gtk.TextView()
tv.set_size_request(400, 200)
tv.get_accessible().set_name("Notes")
hb.pack_start(tv, False, False, 0)
box.pack_start(hb, False, False, 0)

ob = Gtk.Button(label="Open dialog")
ob.connect("clicked", lambda *_: open_dialog())
mb = Gtk.Button(label="Open message")
mb.connect("clicked", lambda *_: open_message())
bb = Gtk.Box(spacing=6, halign=Gtk.Align.START)
bb.pack_start(ob, False, False, 0)
bb.pack_start(mb, False, False, 0)
box.pack_start(bb, False, False, 0)


def open_dialog():
    """A resizable dialog: labwc maximizes it, so its boxes are screen pixels."""
    d = Gtk.Dialog(title="CU Confirm", transient_for=win, modal=True)
    d.add_button("Cancel", Gtk.ResponseType.CANCEL)
    d.add_button("Confirm", Gtk.ResponseType.OK)
    d.get_content_area().add(Gtk.Label(label="Confirm the lab action?"))
    d.connect("response", lambda d, r: (log.write("dialog:confirm\n" if r == Gtk.ResponseType.OK else "dialog:cancel\n"), d.destroy()))
    d.show_all()


def open_message():
    """A fixed-size message dialog: it cannot be maximized."""
    d = Gtk.MessageDialog(transient_for=win, modal=True, message_type=Gtk.MessageType.INFO,
                          buttons=Gtk.ButtonsType.OK, text="CU Message")
    d.connect("response", lambda d, r: (log.write("message:ok\n"), d.destroy()))
    d.show_all()


win.add(box)
win.connect("destroy", Gtk.main_quit)
win.show_all()
Gtk.main()
