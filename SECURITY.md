# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub security advisories:
**[Report a vulnerability](https://github.com/AICodeKing/pandaos/security/advisories/new)**
(the repo's *Security* tab, then *Report a vulnerability*).

Do not open a public issue, pull request or discussion for a security problem.

Include what you can:

- the PandaOS version or commit (`cat /etc/pandaos/VERSION`, or the pinned revision in `/etc/pandaos/flake.lock`)
- the profile (headless or desktop) and which component is affected (`panda-agentd`, `panda-mem`,
  `panda-install`, `panda-browser`, updates, the desktop)
- steps to reproduce, and what an attacker gains

We aim to acknowledge a report within 7 days, agree on a fix and disclosure date with you, and credit
you in the advisory unless you ask us not to.

## Supported versions

PandaOS is pre-1.0. Only the latest release gets security fixes, by way of a new release.
Installed boxes track the `release` branch (`github:AICodeKing/pandaos/release`) and update daily, so
a fix reaches them on the first daily update after the fixed release is tagged, unless the owner has
set an update hold (`panda-update hold`).

**`v0.2.0` is the current supported release.** Fixes land on `main` first and reach installed boxes
through the next signed release on the `release` branch.

| Version | Supported |
|---|---|
| 0.2.0 (latest release) | yes |
| `main` | fixes land here first; not a supported install channel |
| older releases | no, update |
| 0.1.0 | no: it has a well-known default password |

## Release signing key

Installed boxes accept a channel update only if the `release` commit is SSH-signed by a key in
`pandaos.updates.allowedSigners` (see [signed releases](docs/updates-and-rollback.md#signed-releases-required)).
This is the published PandaOS release-signing key, as an `allowed_signers` line:

```text
release@pandaos namespaces="git" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIO8zyQo+fbC0tMzI4gvW6ZICS0p8OVcmeYNGJREXBlNG
```

SHA-256 fingerprint: `SHA256:YSNtMOh+z7ZRA5E0sH2yAtbY+uxUP2cCbEzpbqa3L3o`

It matches the key committed in `nixos/modules/release-signers`, which is pre-configured as the
default of `pandaos.updates.allowedSigners` starting in v0.2.0. The release workflow
(`.github/workflows/release.yml`) runs `git verify-commit` against it before it updates
`origin/release`. Check the fingerprint independently with:

```sh
printf '%s\n' 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIO8zyQo+fbC0tMzI4gvW6ZICS0p8OVcmeYNGJREXBlNG' | ssh-keygen -lf -
```

To override, rotate or disable this trust anchor on a box, see
[signed releases](docs/updates-and-rollback.md#signed-releases-required).

## Scope

The threat model is in [docs/security.md](docs/security.md). Note that the relay protocol in
`panda-agentd` is a placeholder until PandaConnect exists; issues in it are still in scope.
