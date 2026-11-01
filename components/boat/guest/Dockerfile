# boat guest rootfs. Two targets: `slim` (headless dev box) and `full`
# (slim + XFCE/KasmVNC desktop, Chrome, code-server). build.sh exports the
# container filesystem into an ext4 image; nothing here runs as a container.
# Every download is pinned by version + sha256. Bump them together.

FROM ubuntu:24.04@sha256:fe81560990fae86dc4743202b2cb4d3df98d0bcf949173cecef15ce1127ef852 AS slim

ARG NODE_VERSION=26.13.0
ARG NODE_SHA256=d28a96af6c38596f6d9d97c0ee531c13d238d757742387d1928de822d959e997
ARG BUN_VERSION=1.4.4
ARG BUN_SHA256=bf8ccbfb2b0b80159e0f9a6d15c55317add8077534dbc6130a3edb52fccb395c
ARG RUSTUP_VERSION=1.29.2
ARG RUSTUP_SHA256=5be191e8c70a5007074254cb74d78dbfdd25cd5a61c27de7b8e5410122ec24f4
ARG CLAUDE_CODE_VERSION=2.1.316
ARG CODEX_VERSION=0.166.0
ARG OPENCODE_VERSION=1.19.5

ENV DEBIAN_FRONTEND=noninteractive
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]

# Size: no docs/man/info/translations from any package (copyright files stay).
RUN printf '%s\n' 'path-exclude=/usr/share/doc/*' 'path-include=/usr/share/doc/*/copyright' \
      'path-exclude=/usr/share/man/*' 'path-exclude=/usr/share/info/*' 'path-exclude=/usr/share/groff/*' \
      'path-exclude=/usr/share/lintian/*' 'path-exclude=/usr/share/linda/*' \
      'path-exclude=/usr/share/locale/*' 'path-include=/usr/share/locale/locale.alias' \
      > /etc/dpkg/dpkg.cfg.d/boat-excludes

RUN apt-get update && apt-get install -y --no-install-recommends \
      systemd systemd-sysv dbus udev kmod iproute2 iputils-ping iptables \
      ca-certificates curl wget gnupg openssh-server openssh-client sudo \
      e2fsprogs fuse3 procps psmisc lsof less nano vim-tiny file rsync \
      netcat-openbsd unzip zip xz-utils bash-completion tzdata \
      tmux ripgrep jq git gh build-essential pkg-config \
      python3 python3-pip python3-venv \
      docker.io docker-compose-v2 \
 && apt-get clean && rm -rf /var/lib/apt/lists/*

# Node.js LTS (+ npm) from the official tarball.
RUN curl -fsSLo /tmp/node.tar.xz https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.xz \
 && echo "${NODE_SHA256}  /tmp/node.tar.xz" | sha256sum -c - \
 && tar -xJf /tmp/node.tar.xz -C /usr/local --strip-components=1 --exclude='*.md' --exclude=LICENSE \
 && rm /tmp/node.tar.xz && node --version && npm --version

# Bun.
RUN curl -fsSLo /tmp/bun.zip https://github.com/oven-sh/bun/releases/download/bun-v${BUN_VERSION}/bun-linux-x64.zip \
 && echo "${BUN_SHA256}  /tmp/bun.zip" | sha256sum -c - \
 && unzip -j /tmp/bun.zip 'bun-linux-x64/bun' -d /usr/local/bin && ln -s bun /usr/local/bin/bunx \
 && rm /tmp/bun.zip && bun --version

# Agent CLIs. npm installs every libc/CPU-matching variant (opencode: x64 and
# x64-baseline, 214 MB each); postinstall hardlinks the chosen one into bin/,
# so the unused variants are deleted.
RUN npm install -g --no-fund --no-audit \
      @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} \
      @openai/codex@${CODEX_VERSION} \
      opencode-ai@${OPENCODE_VERSION} \
 && npm cache clean --force \
 && find /usr/local/lib/node_modules -type d \( -name 'opencode-linux-x64-baseline*' \
      -o -name 'opencode-linux-x64-musl' -o -name 'claude-code-linux-x64-musl' \) -prune -exec rm -rf {} + \
 && claude --version && codex --version && opencode --version

# User `dev` (uid 1000): passwordless sudo, docker group, ssh keys only.
# The Ubuntu image ships a uid-1000 `ubuntu` user; drop it.
RUN userdel -r ubuntu 2>/dev/null || true \
 && useradd -m -u 1000 -s /bin/bash -G sudo,docker dev \
 && usermod -p '*' dev \
 && install -d -m 700 -o dev -g dev /home/dev/.ssh

# Rust (rustup, stable, minimal profile) for dev.
RUN curl -fsSLo /tmp/rustup-init https://static.rust-lang.org/rustup/archive/${RUSTUP_VERSION}/x86_64-unknown-linux-gnu/rustup-init \
 && echo "${RUSTUP_SHA256}  /tmp/rustup-init" | sha256sum -c - \
 && chmod 755 /tmp/rustup-init \
 && su dev -c '/tmp/rustup-init -y --no-modify-path --profile minimal --default-toolchain stable' \
 && rm -rf /tmp/rustup-init /home/dev/.rustup/downloads /home/dev/.rustup/tmp \
 && su dev -c '~/.cargo/bin/rustc --version'

COPY rootfs/ /
COPY agent/boat-agent.py /usr/local/lib/boat/boat-agent.py

RUN chmod 0440 /etc/sudoers.d/dev && visudo -cf /etc/sudoers.d/dev \
 && chmod 755 /usr/local/lib/boat/boat-agent.py \
 && install -d -m 755 /etc/boat \
 && echo 'export PATH="$HOME/.cargo/bin:$HOME/.local/bin:$PATH"' >> /home/dev/.profile \
 && printf '\nif [ -r /etc/boat/env ]; then . /etc/boat/env; fi\n' >> /etc/bash.bashrc \
 && mkdir -p /etc/systemd/network && ln -sf /dev/null /etc/systemd/network/99-default.link \
 && systemctl enable boat-agent.service boat-growfs.service docker.service ssh.service \
 && (systemctl disable ssh.socket 2>/dev/null || true) \
 && systemctl mask apt-daily.timer apt-daily-upgrade.timer motd-news.timer \
      e2scrub_reap.service systemd-networkd-wait-online.service cloud-init.target \
 && systemctl set-default multi-user.target \
 && rm -f /etc/ssh/ssh_host_* && : > /etc/machine-id \
 && rm -rf /var/lib/apt/lists/* /var/cache/* /var/log/*.log /tmp/* /root/.npm /root/.cache /home/dev/.cache

# ------------------------------------------------------------------ full
FROM slim AS full

ARG KASMVNC_VERSION=1.5.2
ARG KASMVNC_SHA256=89a5d1fbe9e032b122202be158a2b25f15c97e0a5b8dcf92a5e6835c7b7b2acd
ARG CODE_SERVER_VERSION=4.144.0
ARG CODE_SERVER_SHA256=45f5533b502edac68b7eb09c5cec44fd281f80264af9a87fde0bfb2c5e190615

ARG GHOSTTY_DEB=ghostty_1.3.1-0.ppa2_amd64_24.04.deb
ARG GHOSTTY_URL=https://github.com/mkasberg/ghostty-ubuntu/releases/download/1.3.1-0-ppa2
ARG GHOSTTY_SHA256=478d440153ef544426418efc7d6d8901715359f452c46be29071901a94b8cd47

# XFCE (no recommends: no screensaver/locker), terminals, idle probe, fonts,
# Mesa (llvmpipe: software GL 4.5 for Ghostty/glxgears under Xvnc), and the
# fps test helpers (glxgears, xdotool).
# Ghostty: no official build for Ubuntu 24.04 (Ubuntu ships it from 26.04; the
# snap needs snapd), so this is the community .deb the Ghostty docs list,
# pinned by sha256.
RUN apt-get update && apt-get install -y --no-install-recommends \
      xfce4-session xfwm4 xfce4-panel xfdesktop4 xfce4-settings thunar \
      xfce4-terminal dbus-x11 x11-utils x11-xserver-utils xprintidle \
      fonts-dejavu-core fonts-liberation adwaita-icon-theme \
      libgl1-mesa-dri libglx-mesa0 libegl-mesa0 mesa-utils xdotool \
 && curl -fsSLo /tmp/ghostty.deb "${GHOSTTY_URL}/${GHOSTTY_DEB}" \
 && echo "${GHOSTTY_SHA256}  /tmp/ghostty.deb" | sha256sum -c - \
 && apt-get install -y --no-install-recommends /tmp/ghostty.deb \
 && ghostty --version \
 && curl -fsSLo /tmp/kasmvnc.deb https://github.com/kasmtech/KasmVNC/releases/download/v${KASMVNC_VERSION}/kasmvncserver_ubuntu_noble_${KASMVNC_VERSION}_amd64.deb \
 && echo "${KASMVNC_SHA256}  /tmp/kasmvnc.deb" | sha256sum -c - \
 && apt-get install -y --no-install-recommends /tmp/kasmvnc.deb \
 && curl -fsSLo /tmp/code-server.deb https://github.com/coder/code-server/releases/download/v${CODE_SERVER_VERSION}/code-server_${CODE_SERVER_VERSION}_amd64.deb \
 && echo "${CODE_SERVER_SHA256}  /tmp/code-server.deb" | sha256sum -c - \
 && apt-get install -y --no-install-recommends /tmp/code-server.deb \
 && apt-get clean && rm -rf /var/lib/apt/lists/* /tmp/*.deb

# Google Chrome stable (apt repo; the version lands in the image manifest).
RUN curl -fsSL https://dl.google.com/linux/linux_signing_key.pub | gpg --dearmor -o /usr/share/keyrings/google-chrome.gpg \
 && echo 'deb [arch=amd64 signed-by=/usr/share/keyrings/google-chrome.gpg] https://dl.google.com/linux/chrome/deb/ stable main' > /etc/apt/sources.list.d/google-chrome.list \
 && apt-get update && apt-get install -y --no-install-recommends google-chrome-stable \
 && apt-get clean && rm -rf /var/lib/apt/lists/* \
 && google-chrome --version

COPY rootfs-full/ /
RUN chmod 755 /usr/local/bin/boat-desktop-session /usr/local/bin/boat-desktop-animate \
 && rm -rf /var/cache/* /var/log/*.log /tmp/*
