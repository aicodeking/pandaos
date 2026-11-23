# T3 Code and T3 Code Connect

> **New in Unreleased.**

PandaOS ships a first-class host for [T3 Code](https://t3.codes) (v0.1.11) next to PandaCode
Connect. `services.t3code-host` is on by default but **idle until you enable it**: no process,
0 idle RAM and no listening port until you sign in or start it. Once running, the host listens
on `127.0.0.1:3773` by default.

There are three ways to reach it:

| Option | Needs | Cloud relay | Port forwarding |
|---|---|---|---|
| [1. T3 Code Connect](#option-1-t3-code-connect-recommended) (recommended) | a T3 Code account | yes | none |
| [2. SSH tunnel](#option-2-direct-access-over-an-ssh-tunnel) | ssh to the box | no | `ssh -L` |
| [3. Tailscale Serve](#option-3-direct-access-over-tailscale-serve) | Tailscale (not installed by default) with MagicDNS and HTTPS | no | none (tailnet only) |

Both `t3-connect` (the control CLI) and `t3` (T3 Code itself) are on the system `PATH`.

## Option 1: T3 Code Connect (recommended)

T3 Code Connect links the box to your T3 Code account. The box dials out, so it works behind NAT
with no port forwarding, and shows up automatically in the T3 Code desktop app, the mobile app
and <https://app.t3.codes>.

```sh
sudo t3-connect sign-in
```

It prints a device-code link and code. Open the link on your phone or laptop, check that the code
matches, and approve. The sign-in runs as `t3code-sign-in.service` (headless OAuth device flow);
once approved it writes the enabled marker and starts `t3code-host.service`.

If a local host is already running (`T3: local`), `sign-in` stops it, and the agent sessions it
runs, while linking. If sign-in fails, the local host is started again automatically. Pressing
Ctrl-C only detaches from the log stream: `t3code-sign-in.service` keeps running and the code
stays valid. `sudo t3-connect stop` cancels a pending sign-in and leaves the host stopped;
`sudo t3-connect start` (or `restart` or `pair`) cancels it and starts the host right back,
keeping the enabled marker.

Over ssh without a tty, or from a script, start it without waiting and read the link from the
journal:

```sh
sudo t3-connect sign-in --no-wait
sudo journalctl -u t3code-sign-in -o cat -b
```

`sudo t3-connect sign-in --publish-only` links the box to publish agent activity to T3 Connect
without opening the remote Connect relay tunnel. Other devices connect directly over LAN, an SSH
tunnel or Tailscale.

When a new device connects and asks for approval, approve it on the box:

```sh
sudo t3-connect devices            # list connected and pending devices
sudo t3-connect approve <CODE>     # approve the code the device shows
```

Check the result:

```sh
t3-connect status                  # add --json for scripts; --live (root) checks cloud reachability
```

## Option 2: Direct access over an SSH tunnel

No cloud account or relay: the host stays on loopback and you reach it through ssh.

If this box was previously signed in to T3 Code Connect, run `sudo t3-connect unlink` first so it
serves locally without the Connect relay.

1. On the box, start the host and create a one-time pairing link:

   ```sh
   sudo t3-connect start
   sudo t3-connect pair               # --ttl 15m, --label laptop are optional
   ```

   `pair` starts the service if needed and prints the link only in your terminal. The token is
   never written to the journal. Treat the link like a password until it is used or expires.
2. On your laptop, forward the port:

   ```sh
   ssh -L 3773:127.0.0.1:3773 <user>@<box>
   ```

3. Open the pairing link (it points at `http://127.0.0.1:3773`) in your browser or the T3 Code
   desktop app. After pairing, use `http://127.0.0.1:3773` while the tunnel is up.

## Option 3: Direct access over Tailscale Serve

**Tailscale is not installed by default on PandaOS.** Enable it first in
`/etc/pandaos/extra.nix` (never edit `local.nix`), rebuild, and join the box to your tailnet:

```nix
{ ... }: { services.tailscale.enable = true; }
```

```sh
sudo nixos-rebuild switch --flake /etc/pandaos#pandaos
sudo tailscale up                  # prints a login link; approve it in your tailnet
```

The box and your devices must be on the same tailnet, with **MagicDNS** and **HTTPS
certificates** enabled for the tailnet. Then use one of these setups. `t3-connect pair` runs
`t3 pair` as `agent`, which cannot reconfigure Tailscale by default.

**Recommended.** Configure Serve as root and keep `agent` unprivileged. `pair` starts the host
if needed and prints a one-time pairing link.

```sh
sudo tailscale serve --bg http://127.0.0.1:3773
sudo t3-connect pair
```

In the printed pairing link, replace `http://127.0.0.1:3773` with your
`https://<box>.<tailnet>.ts.net` URL. Keep the pairing path and token, then open the link.

**Automatic (optional, less isolated).** Grant `agent` Tailscale operator access once, then let
`t3 pair --tailscale` configure Serve and print a tailnet pairing link.

> **Security warning.** `--operator=agent` lets *every* process running as `agent` (every T3 Code
> session, PandaCode task and agent CLI, including a prompt-injected one) reconfigure `tailscaled`:
> it can expose any local port to the tailnet with Serve or to the internet with Funnel, change
> Serve settings, or log the node out of the tailnet. Prefer the root-managed Serve setup above
> unless you accept that risk. Undo it with `sudo tailscale set --operator=`.

```sh
sudo tailscale set --operator=agent
sudo t3-connect pair --tailscale
```

Add `--tailscale-serve-port 8443` to the pairing command to select another HTTPS port.

## Commands

| Command | What |
|---|---|
| `sudo t3-connect sign-in [--publish-only] [--no-wait]` | Link the box to T3 Code Connect (OAuth device flow) |
| `sudo t3-connect start` / `stop` / `restart` | Run or stop `t3code-host.service` on `127.0.0.1:3773`, with or without Connect. `stop` also cancels a pending `t3code-sign-in.service` and keeps the Connect mode, so a later `start` resumes it |
| `sudo t3-connect pair [--ttl DURATION] [--label LABEL] [--tailscale] [--tailscale-serve-port PORT]` | Print a one-time pairing link in this terminal |
| `sudo t3-connect devices` | List connected and pending T3 Connect devices |
| `sudo t3-connect approve <CODE>` | Approve a connecting device |
| `sudo t3-connect unlink` | Unlink the box from T3 Connect, clear the enabled marker and stop `t3code-host.service`. Keeps the stored T3 sign-in. Run `sudo t3-connect start` to resume local serving over SSH or Tailscale |
| `sudo t3-connect sign-out` | Stop the host, clear the enabled marker and log out of T3 Connect |
| `t3-connect status [--json] [--live]` | State; `--live` needs root and checks cloud reachability |
| `t3-connect logs [journalctl args...]` | `t3code-host.service` logs, for example `t3-connect logs -f` |

`t3-connect status --json` prints one compact line, for example:

```json
{"enabled":true,"signedIn":true,"mode":"connect","service":"active","host":"127.0.0.1","port":3773,"version":"0.1.11","runningVersion":"0.1.11"}
```

`service` is `active`, `activating`, `inactive` or `failed`. The `mode` values are `connect`
(T3 Connect relay), `publish-only` (T3 Connect activity-only), `local` (serving directly without
Connect) and `off` (disabled). `version` is the installed T3 Code; `runningVersion` is the version
the running host was started with, and `null` when the host is stopped. After an update, plain
`t3-connect status` warns when the two differ; run `sudo t3-connect restart` to pick up the new
version.

`sudo t3-connect status --json --live` adds a `cloud` key: the JSON object from
`t3 connect status --json`, or `null` if it is unavailable. After `sudo t3-connect unlink`,
`status` reports `signedIn: false`: the box is no longer linked to T3 Connect, even though the
stored T3 login remains in `/var/lib/agent/.t3`.

**Stop and start keep the mode.** `sudo t3-connect stop` stops the host (and cancels a pending
`t3code-sign-in.service`) but remembers the Connect mode (`last-mode`), so a later
`sudo t3-connect start` resumes Connect or publish-only without signing in again. To switch to
local-only serving, run `sudo t3-connect unlink`, then `sudo t3-connect start`.

`jq` is not installed on headless PandaOS. Check it with `grep`:

```sh
t3_json=$(t3-connect status --json)
grep -q '"signedIn":true' <<<"$t3_json" && grep -q '"service":"active"' <<<"$t3_json"
```

On the desktop, the top bar shows `T3: sign in`, `T3: local` (running without Connect),
`T3: connected`, `T3: starting`, `T3: stopped`, `T3: error` or `T3: ?` (the status could not be
read or parsed), and `panda-menu` (`Super+X`) has a *T3 Code Connect* submenu. See
[desktop.md](desktop.md).

## With or instead of PandaCode Connect

T3 Code and PandaCode Connect can run side by side. First boot offers PandaCode Connect (`p`),
T3 Code Connect (`t`), both (`b`) or neither (`N`). To use only T3 Code, turn the PandaCode host
off in `/etc/pandaos/extra.nix` (never edit `local.nix`) and rebuild:

```nix
{ ... }: { services.pandacode-host.enable = false; }
```

```sh
sudo nixos-rebuild switch --flake /etc/pandaos#pandaos
```

Both hosts run as the same non-wheel `agent` user, so T3 Code sessions get the same box as
PandaCode tasks:

- home `/var/lib/agent`, with workspaces under `~/work` checkpointed by `panda-ckpt`
  ([checkpoints.md](checkpoints.md));
- shared memory at `/mem` through `panda-mem`;
- the MCP registry in `/etc/panda/mcp.d`, synced into the agent CLIs;
- the installed agent CLIs (`claude`, `codex`, `gemini`, `kimi`, `opencode`). Sign them in as `agent`
  (`sudo -iu agent claude`), see [agents.md](agents.md#agent-clis).

## Security model

- **Loopback by default.** The host binds to `127.0.0.1:3773`. `lan` and `openFirewall` are off by
  default; remote access goes through the outbound Connect relay, an SSH tunnel or Tailscale Serve.
- **No tokens in the journal.** The unit redacts the startup `Token:` and `Pairing URL:` lines
  from journald, and `t3-connect pair` prints its link only to your terminal. This keeps pairing
  secrets away from journal readers (for example the `panda-sys` MCP server or `t3-connect logs`),
  not away from processes running as `agent` (see the next point).
- **Non-wheel user, shared trust boundary.** The host and every agent it starts run as `agent`,
  which cannot `sudo` or read your home. T3 Code sessions, PandaCode tasks, the agent CLI logins and
  T3 Code's own credentials in `/var/lib/agent/.t3` all share that one user. A prompt-injected agent
  in either host can therefore run `t3` (for example `t3 pair`) or read `/var/lib/agent/.t3`. This is
  the same trust boundary as `pandacode-host` and the agent CLI logins: everything that runs as
  `agent` is trusted as much as the least trusted task. Neither host can `sudo` or touch the owner's
  home. The box-wide `dev.tty.legacy_tiocsti = 0` sysctl stops an agent process from injecting
  keystrokes into a terminal it shares, for example after `sudo -iu agent`.
- **Sandboxed helper commands.** `t3-connect` runs its `t3` helper commands (`pair`, `devices`,
  `approve`, `unlink`, `sign-out`, `status --live`) through `systemd-run --pipe`, as the same
  `agent` user in `agents.slice` with the same systemd sandbox as the host. The caller's TTY is
  never passed to `agent`, and terminal control characters are stripped from the output.
- **Contained runs.** `t3code-host.service` runs in `agents.slice` with `Delegate=yes`, so each
  task gets its own cgroup and the cgroup OOM killer takes the task, not the host or the box.
- **Rebuilds do not kill sessions.** `restartIfChanged = false`: a `nixos-rebuild` or update does
  not restart a running host. Use `sudo t3-connect restart` when you want the new version.
- T3 Code keeps its state and sign-in in `/var/lib/agent/.t3` (`T3CODE_HOME`), owned by `agent`.
  The enabled marker is `/var/lib/t3code-host/enabled`.

## NixOS options

```nix
services.t3code-host = {
  enable = true;            # on by default; idle until signed in or started
  user = "agent";           # shared with services.pandacode-host
  # host defaults to "127.0.0.1" with lan = false and null with lan = true
  port = 3773;
  lan = false;              # true enables LAN binding when host is null
  openFirewall = false;     # opens `port` regardless of lan; only useful with lan = true or a non-loopback host
};
```

| Option | Default | What |
|---|---|---|
| `enable` | `true` | Install the host, `t3-connect` and `t3`. Idle until enabled at runtime |
| `user` | `"agent"` | User the host and its agents run as. Never a login or `wheel` user |
| `host` | `"127.0.0.1"` when `lan = false`, `null` when `lan = true` | Explicit bind address. With `null`, T3 Code selects the address |
| `port` | `3773` | Listen port |
| `lan` | `false` | Set `T3CODE_LAN=true` so T3 Code binds `0.0.0.0` for private-network or LAN access when `host` is `null` |
| `openFirewall` | `false` | Open `port` in the firewall |
| `package` | T3 Code 0.1.11 | The T3 Code package |
| `cloudflaredPackage` | `cloudflared` 2026.10.4 (static) | Tunnel binary for Connect, passed as `T3CODE_CLOUDFLARED_PATH` |

Put overrides in `/etc/pandaos/extra.nix` and rebuild. Do not put tokens there; it is copied into
the world-readable `/nix/store`.

## Troubleshooting

| Symptom | Check |
|---|---|
| Box does not appear in T3 Code | `t3-connect status`: `signedIn` must be `true` and `service` `active`. Run `sudo t3-connect status --live` for cloud reachability, then `sudo journalctl -u t3code-sign-in -b` |
| Sign-in code expired | Run `sudo t3-connect sign-in` again |
| `service` is `inactive` | Not enabled yet, or stopped with `sudo t3-connect stop`: run `sudo t3-connect start` (resumes the last mode) or `sudo t3-connect sign-in` |
| `status` warns the running version differs | The host still runs the version from before an update (`restartIfChanged = false`). Run `sudo t3-connect restart` (ends running sessions) |
| `service` is `failed` | `t3-connect logs -b`, then `sudo t3-connect restart` |
| Device stuck waiting for approval | `sudo t3-connect devices`, then `sudo t3-connect approve <CODE>` |
| Tunnel shows connection refused | The host is not running (`sudo t3-connect start`), or the local port 3773 is taken: use `ssh -L 13773:127.0.0.1:3773 ...` and `http://127.0.0.1:13773` |
| Pairing link rejected | Links are one-time and expire (`--ttl`). Run `sudo t3-connect pair` again |
| `pair --tailscale` fails | Check `tailscale status`, MagicDNS, HTTPS certificates and `agent` operator access. See [Tailscale Serve](#option-3-direct-access-over-tailscale-serve) for the root-managed setup |
| New version not running after update | `restartIfChanged = false`; run `sudo t3-connect restart` (ends running sessions) |
| Stop using Connect but keep local access | `sudo t3-connect unlink` clears the enabled marker and stops `t3code-host.service` while keeping the stored T3 sign-in. Run `sudo t3-connect start` to resume local access over SSH or Tailscale |
| Remove everything | `sudo t3-connect sign-out` |
