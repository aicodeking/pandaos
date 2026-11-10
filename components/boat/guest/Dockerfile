# boat guest rootfs. Two targets: `slim` (headless dev box) and `full`
# (slim + XFCE/KasmVNC desktop, Chrome, code-server). build.sh exports the
# container filesystem into an ext4 image; nothing here runs as a container.
# Every download is pinned by version + sha256. Bump them together.
# build.sh passes components/boat/cli as the named build context `cli`.

# The boat CLI for use inside a VM, and as the `host` command (a symlink).
FROM golang:1.25-bookworm@sha256:3b4a11519ad929d1e1d261a12cff056f0c85b735253d7d861346b9c6f8b36437 AS boat-cli
COPY --from=cli . /src
RUN cd /src && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o /boat .

# Ruby for the full image, built from the release tarball (no official binary
# for Linux; Ubuntu 24.04 ships 3.2). Only /opt/ruby is copied into `full`.
FROM ubuntu:24.04@sha256:fe81560990fae86dc4743202b2cb4d3df98d0bcf949173cecef15ce1127ef852 AS ruby-build
ARG RUBY_VERSION=4.0.1
ARG RUBY_SHA256=3924be2d05db30f4e35f859bf028be85f4b7dd01714142fd823e4af5de2faf9d
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates curl build-essential libssl-dev libyaml-dev zlib1g-dev libffi-dev libgmp-dev \
 && curl -fsSLo /tmp/ruby.tgz "https://cache.ruby-lang.org/pub/ruby/${RUBY_VERSION%.*}/ruby-${RUBY_VERSION}.tar.gz" \
 && echo "${RUBY_SHA256}  /tmp/ruby.tgz" | sha256sum -c - \
 && mkdir /tmp/ruby && tar -xzf /tmp/ruby.tgz -C /tmp/ruby --strip-components=1 \
 && cd /tmp/ruby && ./configure --prefix=/opt/ruby --disable-install-doc --enable-shared \
 && make -j"$(nproc)" && make install \
 && /opt/ruby/bin/ruby -ropenssl -rpsych -rzlib -e 'puts RUBY_DESCRIPTION'

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
COPY agent/boat-agent.py agent/boat_ops.py /usr/local/lib/boat/
COPY --from=boat-cli /boat /usr/local/bin/boat

RUN ln -s boat /usr/local/bin/host \
 && chmod 0440 /etc/sudoers.d/dev && visudo -cf /etc/sudoers.d/dev \
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

ARG PRIME_AGENT_VERSION=0.10.2
ARG PRIME_AGENT_SHA256=09659961dfde201cc39f1e69b607385d708da3e591f267a4fea04970afb8b005

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
      libavcodec60 libavformat60 libavutil58 libswscale7 \
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
# KasmVNC dlopens unversioned libav*.so for H.264 and silently has none without
# them. The web client gets our defaults (boat-defaults.js) before it starts.
RUN cd /usr/lib/x86_64-linux-gnu \
 && ln -s libavcodec.so.60 libavcodec.so && ln -s libavformat.so.60 libavformat.so \
 && ln -s libavutil.so.58 libavutil.so && ln -s libswscale.so.7 libswscale.so \
 && test -e libavcodec.so && test -e libavformat.so && test -e libavutil.so && test -e libswscale.so \
 && cd /usr/share/kasmvnc/www \
 && sed -i 's#<head>#<head><script src="boat-defaults.js"></script>#' index.html vnc.html \
 && grep -q boat-defaults index.html && grep -q boat-defaults vnc.html
# The other four harnesses boat.dev ships. Each is pinned with its dependencies
# and their checksums: npm lockfiles and a pip hash lock under
# rootfs-full/opt/boat-harness, a sha256 for Prime Agent's release tarball.
RUN H=/opt/boat-harness \
 && for h in pi kimi; do (cd "$H/$h" && npm ci --no-fund --no-audit --ignore-scripts --omit=dev); done \
 && ln -s "$H/pi/node_modules/.bin/pi" /usr/local/bin/pi \
 && ln -s "$H/kimi/node_modules/.bin/kimi" /usr/local/bin/kimi \
 && curl -fsSLo /tmp/prime-agent.tgz https://github.com/PrimeIntellect-ai/prime-agent/releases/download/v${PRIME_AGENT_VERSION}/prime-agent-${PRIME_AGENT_VERSION}-linux-x64.tar.gz \
 && echo "${PRIME_AGENT_SHA256}  /tmp/prime-agent.tgz" | sha256sum -c - \
 && install -d "$H/prime-agent" && tar -xzf /tmp/prime-agent.tgz -C "$H/prime-agent" --no-same-owner \
 && ln -s "$H/prime-agent/prime-agent" /usr/local/bin/prime-agent \
 && python3 -m venv "$H/vibe/venv" \
 && "$H/vibe/venv/bin/pip" install --no-cache-dir --require-hashes --only-binary=:all: -r "$H/vibe/requirements.lock" \
 && ln -s "$H/vibe/venv/bin/vibe" /usr/local/bin/vibe \
 && export HOME=/tmp/harness-home && mkdir "$HOME" \
 && pi --version && kimi --version && prime-agent --version && vibe --version \
 && rm -rf /tmp/harness-home /tmp/prime-agent.tgz /root/.npm /root/.cache
# ---- Languages boat.dev documents as pre-installed (docs.boat.dev/machines/tools).
# Ubuntu packages where Ubuntu 24.04 has the documented version (OpenJDK 11,
# Maven 3.8.7, .NET SDK 8.0.1xx, Erlang/OTP 25, Elixir 1.14, Clang 18, ninja,
# ffmpeg 6.1, wmctrl); every other one is a release artifact pinned by sha256.
# OpenJDK 11 goes in first so maven's `default-jre-headless | java7-runtime-headless`
# is already met and apt does not add OpenJDK 21 (which would win `java`).
ARG GO_VERSION=1.23.4
ARG GO_SHA256=6924efde5de86fe277676e929dc9917d466efa02fb934197bc2eba35d5680971
ARG DENO_VERSION=2.9.4
ARG DENO_SHA256=c24f955d9fbfe0ea5ae2b501c8e71ae76e31e4c9782390a54a284b3364fda725
ARG UV_VERSION=0.12.0
ARG UV_SHA256=eaf842262aa1c418d8ecc5605f02ee1ebfd369124fa48548e85f9481a47831a9
ARG PNPM_VERSION=10.34.5
ARG NVM_VERSION=0.41.0
ARG NVM_SHA256=532560c9ecc6ef484279fcc1c600e9346537bc7913b891ffd0ec0caca5072694
ARG NVM_NODE_VERSION=22.23.4
ARG NVM_NODE_SHA256=1805bcd35e6815559bd2eb97db438d3fdb0eb3a5e6a977b7c1badca76337e9ce
ARG GRADLE_VERSION=8.12
ARG GRADLE_SHA256=7a00d51fb93147819aab76024feece20b6b84e420694101f276be952e08bef03
ARG KOTLIN_VERSION=2.1.0
ARG KOTLIN_SHA256=b6698d5728ad8f9edcdd01617d638073191d8a03139cc538a391b4e3759ad297
ARG SCALA_VERSION=3.3.8
ARG SCALA_SHA256=17a03dd9c2a790b4230a7d03b741cdedca6514096e50e7189bc5e2d299db250a
ARG SBT_VERSION=1.13.0
ARG SBT_SHA256=06806805ffd26232727326216766ed4793b549f8c1e6ffeef2e610db7245b698
ARG CMAKE_VERSION=3.31.3
ARG CMAKE_SHA256=442b93b3db0f6c6501a74add587acca5ea6b8c1526ccca7bc8d89fc592036971
# boat.dev documents PHP 8.4.24; static-php-cli's newest 8.4 build is 8.4.23.
ARG PHP_VERSION=8.4.23
ARG PHP_SHA256=1aeed5bc7967977ca5b1da7163acd91bf9ba3ac56037045d4e91ee2ff2712bb7
ARG COMPOSER_VERSION=2.11.2
ARG COMPOSER_SHA256=dd158c0afd41d15fac89e1b4e47b5d42911fe316c3687cd5edac98319b25d028
ARG R_DEB=r-base-core_4.6.1-9.2404.0_amd64.deb
ARG R_SHA256=e74382f25055a44ad529f6f744c5f394a330c8deee5c2d271ff45fdb4eaa748a

RUN apt-get update \
 && apt-get install -y --no-install-recommends openjdk-11-jdk-headless \
 && apt-get install -y --no-install-recommends maven dotnet-sdk-8.0 elixir \
      clang ninja-build ffmpeg wmctrl libyaml-0-2 \
 && curl -fsSLo "/tmp/${R_DEB}" "https://cloud.r-project.org/bin/linux/ubuntu/noble-cran40/${R_DEB}" \
 && echo "${R_SHA256}  /tmp/${R_DEB}" | sha256sum -c - \
 && apt-get install -y --no-install-recommends "/tmp/${R_DEB}" \
 && apt-get clean && rm -rf /var/lib/apt/lists/* /tmp/*.deb \
 && java -version 2>&1 | grep '"11\.' \
 && mvn --version && dotnet --version && elixir --version && clang --version && R --version | sed -n 1p

COPY --from=ruby-build /opt/ruby /opt/ruby

RUN fetch() { curl -fsSLo "$1" "$2" && echo "$3  $1" | sha256sum -c -; } \
 && fetch /tmp/go.tgz "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" "$GO_SHA256" \
 && tar -xzf /tmp/go.tgz -C /usr/local && ln -s /usr/local/go/bin/go /usr/local/go/bin/gofmt /usr/local/bin/ \
 && fetch /tmp/deno.zip "https://github.com/denoland/deno/releases/download/v${DENO_VERSION}/deno-x86_64-unknown-linux-gnu.zip" "$DENO_SHA256" \
 && unzip -q /tmp/deno.zip deno -d /usr/local/bin \
 && fetch /tmp/uv.tgz "https://github.com/astral-sh/uv/releases/download/${UV_VERSION}/uv-x86_64-unknown-linux-gnu.tar.gz" "$UV_SHA256" \
 && tar -xzf /tmp/uv.tgz -C /usr/local/bin --strip-components=1 --no-same-owner uv-x86_64-unknown-linux-gnu/uv uv-x86_64-unknown-linux-gnu/uvx \
 && fetch /tmp/gradle.zip "https://services.gradle.org/distributions/gradle-${GRADLE_VERSION}-bin.zip" "$GRADLE_SHA256" \
 && unzip -q /tmp/gradle.zip -d /opt && ln -s "/opt/gradle-${GRADLE_VERSION}/bin/gradle" /usr/local/bin/gradle \
 && fetch /tmp/kotlin.zip "https://github.com/JetBrains/kotlin/releases/download/v${KOTLIN_VERSION}/kotlin-compiler-${KOTLIN_VERSION}.zip" "$KOTLIN_SHA256" \
 && unzip -q /tmp/kotlin.zip -d /opt && ln -s /opt/kotlinc/bin/kotlin /opt/kotlinc/bin/kotlinc /usr/local/bin/ \
 && fetch /tmp/scala.tgz "https://github.com/scala/scala3/releases/download/${SCALA_VERSION}/scala3-${SCALA_VERSION}.tar.gz" "$SCALA_SHA256" \
 && install -d /opt/scala3 && tar -xzf /tmp/scala.tgz -C /opt/scala3 --strip-components=1 --no-same-owner \
 && ln -s /opt/scala3/bin/scala /opt/scala3/bin/scalac /opt/scala3/bin/scaladoc /usr/local/bin/ \
 && fetch /tmp/sbt.tgz "https://github.com/sbt/sbt/releases/download/v${SBT_VERSION}/sbt-${SBT_VERSION}.tgz" "$SBT_SHA256" \
 && tar -xzf /tmp/sbt.tgz -C /opt --no-same-owner && ln -s /opt/sbt/bin/sbt /usr/local/bin/sbt \
 && fetch /tmp/cmake.tgz "https://github.com/Kitware/CMake/releases/download/v${CMAKE_VERSION}/cmake-${CMAKE_VERSION}-linux-x86_64.tar.gz" "$CMAKE_SHA256" \
 && install -d /opt/cmake && tar -xzf /tmp/cmake.tgz -C /opt/cmake --strip-components=1 --no-same-owner \
      --exclude='*/doc' --exclude='*/man' \
 && ln -s /opt/cmake/bin/cmake /opt/cmake/bin/ctest /opt/cmake/bin/cpack /usr/local/bin/ \
 && fetch /tmp/php.tgz "https://dl.static-php.dev/static-php-cli/common/php-${PHP_VERSION}-cli-linux-x86_64.tar.gz" "$PHP_SHA256" \
 && tar -xzf /tmp/php.tgz -C /usr/local/bin --no-same-owner php \
 && fetch /usr/local/bin/composer "https://getcomposer.org/download/${COMPOSER_VERSION}/composer.phar" "$COMPOSER_SHA256" \
 && chmod 755 /usr/local/bin/composer \
 && for b in ruby gem bundle bundler irb erb rake rdoc ri; do ln -s "/opt/ruby/bin/$b" /usr/local/bin/; done \
 && npm install -g --no-fund --no-audit "pnpm@${PNPM_VERSION}" && npm cache clean --force \
 && fetch /tmp/nvm.tgz "https://github.com/nvm-sh/nvm/archive/refs/tags/v${NVM_VERSION}.tar.gz" "$NVM_SHA256" \
 && install -d /usr/local/nvm && tar -xzf /tmp/nvm.tgz -C /usr/local/nvm --strip-components=1 --no-same-owner \
 && fetch /tmp/node22.txz "https://nodejs.org/dist/v${NVM_NODE_VERSION}/node-v${NVM_NODE_VERSION}-linux-x64.tar.xz" "$NVM_NODE_SHA256" \
 && install -d "/usr/local/nvm/versions/node/v${NVM_NODE_VERSION}" \
 && tar -xJf /tmp/node22.txz -C "/usr/local/nvm/versions/node/v${NVM_NODE_VERSION}" --strip-components=1 --no-same-owner \
 && chown -R dev:dev /usr/local/nvm \
 && printf '\n[ -r /etc/profile.d/boat-nvm.sh ] && . /etc/profile.d/boat-nvm.sh\n' >> /etc/bash.bashrc \
 && rm -rf /tmp/* /root/.npm /root/.cache \
 && export HOME=/tmp/lang-home && mkdir "$HOME" \
 && go version && deno --version && uv --version && pnpm --version && gradle --version >/dev/null \
 && kotlinc -version && scala -version && sbt --script-version && cmake --version && ninja --version \
 && php --version && COMPOSER_ALLOW_SUPERUSER=1 composer --version --no-interaction && ruby --version && bundle --version \
 && bash -c '. /etc/profile.d/boat-nvm.sh && nvm --version && nvm ls' \
 && [ "$(node --version)" != "v${NVM_NODE_VERSION}" ] \
 && rm -rf /tmp/lang-home

# boat.dev's guest user is `user` (/home/user); ours is `dev`. `user` is a second
# name for uid 1000 (listed after dev, so uid lookups still say dev) and
# /home/user is a symlink to /home/dev: every /home/dev path keeps working.
RUN useradd -o -u 1000 -g dev -G sudo,docker -M -d /home/user -s /bin/bash user \
 && usermod -p '*' user && ln -s dev /home/user \
 && echo 'user ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/user && chmod 0440 /etc/sudoers.d/user \
 && visudo -cf /etc/sudoers.d/user \
 && [ "$(id -un 1000)" = dev ] && [ "$(id -u user)" = 1000 ] && [ "$(getent passwd 1000 | cut -d: -f1)" = dev ]

RUN chmod 755 /usr/local/bin/boat-desktop-session /usr/local/bin/boat-desktop-animate \
 && rm -rf /var/cache/* /var/log/*.log /tmp/*
