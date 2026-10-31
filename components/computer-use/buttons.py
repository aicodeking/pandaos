#!/usr/bin/env python3
"""vmtest app: a 10x8 grid of GTK 3 buttons B0..B79. Each click appends the
button's label to ~/cu-clicks.log, so the test knows what was really hit."""
import os

import gi

gi.require_version("Gtk", "3.0")
from gi.repository import GLib, Gtk  # noqa: E402

GLib.set_prgname("cu-buttons")
log = open(os.path.expanduser("~/cu-clicks.log"), "a", buffering=1)
win = Gtk.Window(title="CU Buttons")
grid = Gtk.Grid(row_homogeneous=True, column_homogeneous=True, row_spacing=8, column_spacing=8, margin=16)
for i in range(80):
    b = Gtk.Button(label=f"B{i}")
    b.connect("clicked", lambda b: log.write(b.get_label() + "\n"))
    grid.attach(b, i % 10, i // 10, 1, 1)
win.add(grid)
win.connect("destroy", Gtk.main_quit)
win.show_all()
Gtk.main()
