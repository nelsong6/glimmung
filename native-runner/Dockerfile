# syntax=docker/dockerfile:1.7

FROM golang:1.25-alpine AS backend
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/glimmung-native-runner ./cmd/glimmung-native-runner

FROM ghcr.io/actions/actions-runner:latest AS runtime

USER root

ENV DEBIAN_FRONTEND=noninteractive \
    PIP_BREAK_SYSTEM_PACKAGES=1 \
    PLAYWRIGHT_BROWSERS_PATH=/ms-playwright \
    PLAYWRIGHT_PACKAGE_PATH=/opt/glimmung-runner/node_modules/playwright/index.mjs

RUN printf '%s\n' \
        'Acquire::Retries "5";' \
        'Acquire::http::Timeout "30";' \
        'Acquire::https::Timeout "30";' \
        'APT::Get::Assume-Yes "true";' \
        'APT::Install-Recommends "false";' \
        'APT::Install-Suggests "false";' \
        >/etc/apt/apt.conf.d/80glimmung-retries \
    && printf '%s\n' \
        '#!/bin/sh' \
        'set -eu' \
        'attempt=1' \
        'while [ "$attempt" -le 5 ]; do' \
        '  if apt-get "$@"; then' \
        '    exit 0' \
        '  fi' \
        '  if [ "$attempt" -eq 5 ]; then' \
        '    exit 1' \
        '  fi' \
        '  sleep $((attempt * 10))' \
        '  attempt=$((attempt + 1))' \
        'done' \
        >/usr/local/bin/apt-retry \
    && chmod +x /usr/local/bin/apt-retry \
    && find /etc/apt -type f \( -name '*.list' -o -name '*.sources' \) -print0 \
        | xargs -0 sed -i \
            -e 's|http://archive.ubuntu.com/ubuntu|http://azure.archive.ubuntu.com/ubuntu|g' \
            -e 's|http://security.ubuntu.com/ubuntu|http://azure.archive.ubuntu.com/ubuntu|g'

RUN apt-retry update \
    && apt-retry install \
        ca-certificates \
        curl \
        git \
        golang-go \
        gnupg \
        jq \
        openssh-client \
        python3 \
        python3-pip \
        python3-venv \
        sudo \
        unzip \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-retry update \
    && apt-retry install nodejs \
    && rm -rf /var/lib/apt/lists/*

RUN echo "runner ALL=(ALL) NOPASSWD:ALL" >/etc/sudoers.d/runner \
    && chmod 0440 /etc/sudoers.d/runner

RUN curl -sL https://aka.ms/InstallAzureCLIDeb | bash

RUN KUBECTL_VERSION="$(curl -fsSL https://dl.k8s.io/release/stable.txt)" \
    && curl -fsSLo /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl" \
    && chmod +x /usr/local/bin/kubectl

RUN curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash

# Tailscale client + tailscaled. Project workflows that need to reach
# a project-pinned host outside the cluster (see
# docs/remote-host-execution.md) bring tailscaled up in userspace
# networking mode per-run; the daemon is not started in the image. The
# install path follows Tailscale's published apt repo so we get signed
# packages without curl|sh.
RUN curl -fsSL https://pkgs.tailscale.com/stable/ubuntu/jammy.noarmor.gpg \
        -o /usr/share/keyrings/tailscale-archive-keyring.gpg \
    && curl -fsSL https://pkgs.tailscale.com/stable/ubuntu/jammy.tailscale-keyring.list \
        -o /etc/apt/sources.list.d/tailscale.list \
    && apt-retry update \
    && apt-retry install tailscale \
    && rm -rf /var/lib/apt/lists/*

RUN mkdir -p -m 755 /etc/apt/keyrings \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
        -o /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && printf 'deb [arch=%s signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main\n' "$(dpkg --print-architecture)" \
        >/etc/apt/sources.list.d/github-cli.list \
    && apt-retry update \
    && apt-retry install gh \
    && rm -rf /var/lib/apt/lists/*

RUN npm install -g @openai/codex @anthropic-ai/claude-code \
    && mkdir -p /opt/glimmung-runner \
    && npm install --prefix /opt/glimmung-runner playwright \
    && npx --prefix /opt/glimmung-runner playwright install --with-deps chromium \
    && chmod -R a+rX /ms-playwright /opt/glimmung-runner \
    && npm cache clean --force \
    && rm -rf /root/.npm /opt/glimmung-runner/.cache

COPY --from=backend /out/glimmung-native-runner /app/glimmung-native-runner
RUN mkdir -p /workspace \
    && chown runner:runner /workspace \
    && chmod +x /app/glimmung-native-runner

USER runner
WORKDIR /workspace
ENTRYPOINT ["/app/glimmung-native-runner"]
