# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub security advisories:
**[Report a vulnerability](https://github.com/AICodeKing/pandaos/security/advisories/new)**
(the repo's *Security* tab, then *Report a vulnerability*).

Do not open a public issue, pull request or discussion for a security problem.

Include what you can:

- the PandaOS version or commit (`cat /etc/pandaos/flake.lock` shows the pinned revision on an installed box)
- the profile (headless or desktop) and which component is affected (`panda-agentd`, `panda-mem`,
  `panda-install`, `panda-browser`, updates, the desktop)
- steps to reproduce, and what an attacker gains

We aim to acknowledge a report within 7 days, agree on a fix and disclosure date with you, and credit
you in the advisory unless you ask us not to.

## Supported versions

PandaOS is pre-1.0. Only the latest release and `main` get security fixes. Installed boxes track
`github:AICodeKing/pandaos` and update daily, so a fix reaches them on the next update after it lands.

| Version | Supported |
|---|---|
| latest release / `main` | yes |
| older releases | no, update |

## Scope

The threat model is in [docs/security.md](docs/security.md). Note that the relay protocol in
`panda-agentd` is a placeholder until PandaConnect exists; issues in it are still in scope.
