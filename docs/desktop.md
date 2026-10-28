# Desktop

The `desktop` profile adds a very light Wayland session: labwc, a waybar top bar and Panda dock,
the `panda-menu` repair menu, a lock screen and notifications. There is no X server and no Xwayland.
Log in at the `tuigreet` greeter.

> The Panda dock, agent status, lock/idle, notifications, laptop keys and the new menu items are
> **new in Unreleased**. v0.1.0 had a bare labwc session.

## GPU: off by default

By default the session uses **software rendering** (labwc's pixman renderer, `WLR_RENDERER=pixman`)
and ships no Mesa drivers. That is enough for this shell and saves about 800 MB of closure (Mesa and
LLVM). To use the GPU, set this in `/etc/pandaos/local.nix` and rebuild:

```nix
pandaos.desktop.gpu = true;   # Mesa (hardware.graphics) and labwc's GL renderer
```

Turn it on if you run GPU-heavy apps or video; the Panda shell itself does not need it.

## Lock, idle and notifications

- `panda-lock` (swaylock) locks the screen. `Super+L` or `panda-menu` → *Lock screen*.
- After 10 minutes idle, and before suspend, the screen locks (swayidle).
- Notifications are shown by `fnott`.

## Keys

| Key | Action |
|---|---|
| `Super+D` | app launcher (fuzzel) |
| `Super+Return` | terminal (foot) |
| `Super+E` | file manager (`lf` in foot) |
| `Super+X` | `panda-menu` |
| `Super+L` | lock |
| `Super+Q` | close window |
| `Super+Left` / `Super+Right` | snap to half of the screen |
| `Super+Up` / `Super+Down` | maximise / minimise |
| Volume and mic-mute keys | `wpctl` (PipeWire) |
| Brightness keys | `brightnessctl` |

## panda-menu

The repair menu, also on the dock's *Repair* button. Every action opens in a terminal that stays
open so you can read the output.

| Item | What it does |
|---|---|
| Network | `nmtui` |
| Disks, Disk usage | `lsblk` and `df`; `ncdu` of `/` |
| Logs, Failed services | `journalctl -f`; `systemctl --failed` |
| Agents status | `panda-agentd status` |
| Pairing code | shows the code from `/run/panda-agentd/pairing-code` (needs `wheel`) |
| Memory browser | lists memories and opens one with `panda-mem read` |
| Restore /mem from snapshot | `panda-rollback-mem <snapshot>`, after a confirmation |
| Updates | `panda-update status`, *Run now*, *Hold*, *Unhold* |
| Roll back system | sets the update hold, then `nixos-rebuild switch --rollback` |
| Lock screen, Reboot, Power off | |

See [updates-and-rollback.md](updates-and-rollback.md#panda-update) for the update hold.
