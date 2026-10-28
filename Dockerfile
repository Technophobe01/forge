# syntax=docker/dockerfile:1
#
# Production image for the Kenn Forge maintainer console (formerly middleman),
# as Technophobe01 deploys it: upstream's frontend and Go build stages as they
# are (pinned Bun and Node, patched CA certificates, Go 1.27.3), plus the
# fleet-hub conversion helper, and a runtime stage that keeps the middleman
# image's contract: the MIDDLEMAN_* settings docker-entrypoint.sh translates,
# uid 10001 owning the existing /data volumes, and socat bridging the external
# port to kenn-forge's loopback listener.
#
# Build:  docker build --build-arg VERSION=<forge tag> -t ghcr.io/technophobe01/middleman:latest .
ARG BUN_IMAGE=oven/bun:1.4.4@sha256:1d22548f177f4ff38e0c15baa3c2c46c6c2060ba2d1bc573bb4720b407548f1d
FROM --platform=$BUILDPLATFORM ${BUN_IMAGE} AS bun
FROM --platform=$BUILDPLATFORM node:24.22.1-bookworm-slim AS frontend
WORKDIR /src
COPY --from=bun /usr/local/bin/bun /usr/local/bin/bun
ENV UV_THREADPOOL_SIZE=2 RAYON_NUM_THREADS=2
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git \
    && dpkg --compare-versions "$(dpkg-query -W -f='${Version}' ca-certificates)" ge 20250419~deb12u2 \
    && rm -rf /var/lib/apt/lists/*
COPY . .
RUN bun install --frozen-lockfile \
    && node scripts/build-frontend.mjs \
    && cd packages/github-app-ui && node ../../node_modules/vite-plus/bin/vp build --logLevel warn

FROM --platform=$BUILDPLATFORM golang:1.27.3-bookworm AS build
WORKDIR /src
# "|" falls back to the source repo on any proxy error, including HTTP/2 stream resets mid-download.
ENV CGO_ENABLED=0 GOFLAGS=-p=2 GOMAXPROCS=2 GOPROXY="https://proxy.golang.org|direct"
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /src/frontend/dist ./internal/web/dist
COPY --from=frontend /src/packages/github-app-ui/dist ./internal/githubapp/ui/dist
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG REVISION=unknown
ARG BUILD_DATE=unknown
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${REVISION} -X main.buildDate=${BUILD_DATE}" \
    -o /out/kenn-forge ./cmd/kenn-forge
# The entrypoint's fleet-hub conversion (MIDDLEMAN_FLEET_ORIGIN).
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" \
    -o /out/kenn-forge-fleet-hub ./docker/fleethub

FROM debian:bookworm-slim
# git/openssh-client/tmux: kenn-forge shells out to git and runs workspace
# sessions; socat bridges 0.0.0.0:<port> to kenn-forge's loopback listener; curl
# is for the healthcheck. The ca-certificates floor is upstream's: images built
# before it trust the compromised eMudhra roots.
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl socat git openssh-client tmux \
    && dpkg --compare-versions "$(dpkg-query -W -f='${Version}' ca-certificates)" ge 20250419~deb12u2 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/kenn-forge /usr/local/bin/kenn-forge
COPY --from=build /out/kenn-forge-fleet-hub /usr/local/bin/kenn-forge-fleet-hub
COPY LICENSE /usr/share/doc/kenn-forge/LICENSE
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

ARG VERSION=dev
ARG REVISION=unknown
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.source="https://github.com/Technophobe01/forge" \
    org.opencontainers.image.version=$VERSION \
    org.opencontainers.image.revision=$REVISION \
    org.opencontainers.image.created=$BUILD_DATE

# Run as a non-root user; the named /data volume inherits this ownership. The
# uid stays 10001, not upstream's 1000, so volumes created by the middleman
# image remain writable.
RUN useradd --system --uid 10001 --home-dir /data --shell /usr/sbin/nologin forge \
 && mkdir -p /data && chown forge:forge /data
USER forge

# Config + SQLite live under MIDDLEMAN_HOME (exported to kenn-forge as
# KENN_FORGE_HOME) so they persist on the volume.
# MIDDLEMAN_PORT is the external (socat) port; kenn-forge itself binds
# MIDDLEMAN_INTERNAL_PORT on loopback inside the container.
ENV MIDDLEMAN_HOME=/data
ENV MIDDLEMAN_PORT=8091
ENV MIDDLEMAN_INTERNAL_PORT=8092
EXPOSE 8091
VOLUME ["/data"]

# Send a loopback X-Forwarded-Host so the check also passes when
# MIDDLEMAN_TRUST_REVERSE_PROXY is enabled (which otherwise requires a forwarded
# host on every request). The host is always in the seeded allowlist; when trust
# is off the header is ignored.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=5 \
  CMD curl -fsS -H "X-Forwarded-Host: 127.0.0.1:${MIDDLEMAN_PORT:-8091}" \
      "http://127.0.0.1:${MIDDLEMAN_PORT:-8091}/healthz" || exit 1

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
