# syntax=docker/dockerfile:1
FROM golang:1.26.3-trixie AS toolchain
FROM --platform=$BUILDPLATFORM golang:1.26.3-trixie AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod main.go github_app.go hosted_*.go ./
RUN CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" go build -trimpath -o /out/coderabbit-plus .

FROM node:26.13.1-trixie-slim
ARG CODEX_VERSION=0.172.0
ARG AST_GREP_VERSION=0.51.3
ARG PNPM_VERSION=12.23.0
RUN apt-get update && apt-get install -y --no-install-recommends \
    bubblewrap ca-certificates cargo gh git jq python3 python3-venv ripgrep \
    && rm -rf /var/lib/apt/lists/* \
    && npm install -g "@openai/codex@${CODEX_VERSION}" "@ast-grep/cli@${AST_GREP_VERSION}" "pnpm@${PNPM_VERSION}" \
    && npm cache clean --force \
    && groupadd --gid 10001 reviewer \
    && useradd --uid 10001 --gid reviewer --create-home reviewer \
    && install -d -o reviewer -g reviewer -m 0700 /codex-home
COPY --from=build /out/coderabbit-plus /usr/local/bin/
COPY --from=toolchain /usr/local/go /usr/local/go
COPY plugins/coderabbit-plus/skills/coderabbit-plus/ /home/reviewer/.agents/skills/coderabbit-plus/
COPY --from=vibe_security / /home/reviewer/.agents/skills/vibe-security/
RUN test -s /home/reviewer/.agents/skills/vibe-security/SKILL.md \
    && test -s /home/reviewer/.agents/skills/vibe-security/references/ai-integration.md \
    && node --version && codex --version && ast-grep --version && pnpm --version
ENV HOME=/home/reviewer \
    CODEX_HOME=/codex-home \
    XDG_CACHE_HOME=/tmp/cache \
    GIT_CONFIG_NOSYSTEM=1 \
    GIT_CONFIG_GLOBAL=/dev/null \
    GIT_TERMINAL_PROMPT=0 \
    CODERABBIT_PLUS_SKILL=/home/reviewer/.agents/skills/coderabbit-plus/SKILL.md \
    PATH="/usr/local/go/bin:${PATH}"
USER 10001:10001
WORKDIR /home/reviewer
ENTRYPOINT ["coderabbit-plus"]
