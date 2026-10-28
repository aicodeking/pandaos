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

**No release has been tagged yet.** Until then, fixes land on `main` only, and installed boxes keep
the version they were installed with.

| Version | Supported |
|---|---|
| latest release | yes |
| `main` | fixes land here first; not a supported install channel |
| older releases | no, update |
| 0.1.0 | no: it has a well-known default password |

## Scope

The threat model is in [docs/security.md](docs/security.md). Note that the relay protocol in
`panda-agentd` is a placeholder until PandaConnect exists; issues in it are still in scope.
