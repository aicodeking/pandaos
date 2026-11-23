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
| [3. Tailscale Serve](#option-3-direct-access-over-tailscale-serve) | Tailscale with MagicDNS and HTTPS | no | none (tailnet only) |

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

If the box and your devices are on the same tailnet, with **MagicDNS** and **HTTPS certificates**
enabled for the tailnet, use one of these setups. `t3-connect pair` runs `t3 pair` as `agent`,
which cannot reconfigure Tailscale by default.

**Recommended.** Configure Serve as root and keep `agent` unprivileged. `pair` starts the host
if needed and prints a one-time pairing link.

```sh
sudo tailscale serve --bg http://127.0.0.1:3773
sudo t3-connect pair
```

In the printed pairing link, replace `http://127.0.0.1:3773` with your
`https://<box>.<tailnet>.ts.net` URL. Keep the pairing path and token, then open the link.

**Automatic.** Grant `agent` Tailscale operator access once, then let `t3 pair --tailscale`
configure Serve and print a tailnet pairing link.

```sh
sudo tailscale set --operator=agent
sudo t3-connect pair --tailscale
```

Add `--tailscale-serve-port 8443` to the pairing command to select another HTTPS port.

## Commands

| Command | What |
|---|---|
| `sudo t3-connect sign-in [--publish-only] [--no-wait]` | Link the box to T3 Code Connect (OAuth device flow) |
| `sudo t3-connect start` / `stop` / `restart` | Run or stop `t3code-host.service` on `127.0.0.1:3773`, with or without Connect |
| `sudo t3-connect pair [--ttl DURATION] [--label LABEL] [--tailscale] [--tailscale-serve-port PORT]` | Print a one-time pairing link in this terminal |
| `sudo t3-connect devices` | List connected and pending T3 Connect devices |
| `sudo t3-connect approve <CODE>` | Approve a connecting device |
| `sudo t3-connect unlink` | Unlink the box from T3 Connect, clear the enabled marker and stop `t3code-host.service`. Keeps the stored T3 sign-in. Run `sudo t3-connect start` to resume local serving over SSH or Tailscale |
| `sudo t3-connect sign-out` | Stop the host, clear the enabled marker and log out of T3 Connect |
| `t3-connect status [--json] [--live]` | State; `--live` needs root and checks cloud reachability |
| `t3-connect logs [journalctl args...]` | `t3code-host.service` logs, for example `t3-connect logs -f` |

`t3-connect status --json` prints one compact line, for example:

```json
{"enabled":true,"signedIn":true,"mode":"connect","service":"active","host":"127.0.0.1","port":3773,"version":"0.1.11"}
```

`service` is `active`, `activating`, `inactive` or `failed`. The `mode` values are `connect`
(T3 Connect relay), `publish-only` (T3 Connect activity-only), `local` (serving directly without
Connect) and `off` (disabled).

`jq` is not installed on headless PandaOS. Check it with `grep`:

```sh
t3_json=$(t3-connect status --json)
grep -q '"signedIn":true' <<<"$t3_json" && grep -q '"service":"active"' <<<"$t3_json"
```

On the desktop, the top bar shows `T3: sign in`, `T3: local` (running without Connect),
`T3: connected`, `T3: starting`, `T3: stopped` or `T3: error`, and `panda-menu` (`Super+X`) has a *T3 Code Connect* submenu. See
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
- the installed agent CLIs (`claude`, `codex`, `gemini`, `opencode`). Sign them in as `agent`
  (`sudo -iu agent claude`), see [agents.md](agents.md#agent-clis).

## Security model

- **Loopback by default.** The host binds to `127.0.0.1:3773`. `lan` and `openFirewall` are off by
  default; remote access goes through the outbound Connect relay, an SSH tunnel or Tailscale Serve.
- **No tokens in the journal.** The unit redacts the startup `Token:` and `Pairing URL:` lines
  from journald, and `t3-connect pair` prints its link only to your terminal.
- **Non-wheel user.** The host and every agent it starts run as `agent`, which cannot `sudo` or
  read your home.
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
  openFirewall = false;     # opens `port` when lan = true
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
| `service` is `inactive` | Not enabled yet: `sudo t3-connect sign-in` or `sudo t3-connect start` |
| `service` is `failed` | `t3-connect logs -b`, then `sudo t3-connect restart` |
| Device stuck waiting for approval | `sudo t3-connect devices`, then `sudo t3-connect approve <CODE>` |
| Tunnel shows connection refused | The host is not running (`sudo t3-connect start`), or the local port 3773 is taken: use `ssh -L 13773:127.0.0.1:3773 ...` and `http://127.0.0.1:13773` |
| Pairing link rejected | Links are one-time and expire (`--ttl`). Run `sudo t3-connect pair` again |
| `pair --tailscale` fails | Check `tailscale status`, MagicDNS, HTTPS certificates and `agent` operator access. See [Tailscale Serve](#option-3-direct-access-over-tailscale-serve) for the root-managed setup |
| New version not running after update | `restartIfChanged = false`; run `sudo t3-connect restart` (ends running sessions) |
| Stop using Connect but keep local access | `sudo t3-connect unlink` clears the enabled marker and stops `t3code-host.service` while keeping the stored T3 sign-in. Run `sudo t3-connect start` to resume local access over SSH or Tailscale |
| Remove everything | `sudo t3-connect sign-out` |
