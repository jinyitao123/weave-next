ARG NPM_REGISTRY=https://registry.npmjs.org
ARG OPENCODE_VERSION=1.18.11
ARG CODEX_VERSION=0.146.0
ARG CLAUDE_CODE_VERSION=2.1.220

# Admin console: built first and embedded into the server binary below.
FROM node:22-slim AS admin
ARG NPM_REGISTRY
WORKDIR /src/web/admin
COPY web/admin/package.json web/admin/package-lock.json ./
RUN --mount=type=cache,id=weave-admin-npm,target=/root/.npm \
    npm ci --no-audit --no-fund --registry="${NPM_REGISTRY}"
COPY web/admin ./
COPY internal/app/adminui/dist/.gitkeep /src/internal/app/adminui/dist/.gitkeep
RUN npm run build

FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /build

COPY go.mod go.sum ./
COPY third_party/loom ./third_party/loom
ARG GOPROXY=https://proxy.golang.org,direct
RUN --mount=type=cache,id=weave-gomod,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,id=weave-gobuild,target=/root/.cache/go-build,sharing=locked \
    go mod download

COPY . .
COPY --from=admin /src/internal/app/adminui/dist ./internal/app/adminui/dist
# Git metadata is excluded by .dockerignore, so the commit must be supplied by
# the host (see `make docker-build`); plain `docker compose build` falls back
# to "unknown".
ARG BUILD_COMMIT=unknown
ARG WEAVE_VERSION=development
RUN --mount=type=cache,id=weave-gomod,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,id=weave-gobuild,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 go build -ldflags "-X main.buildCommit=${BUILD_COMMIT} -X main.buildVersion=${WEAVE_VERSION}" -o /weave ./cmd/weave
RUN --mount=type=cache,id=weave-gomod,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,id=weave-gobuild,target=/root/.cache/go-build,sharing=locked \
    mkdir -p /dist/runtime && \
    for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do \
      os=${target%/*}; arch=${target#*/}; \
      out=/dist/runtime/weave-runtime-$os-$arch; \
      [ "$os" = windows ] && out=$out.exe; \
      CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build \
        -ldflags "-X main.buildCommit=${BUILD_COMMIT} -X main.buildVersion=${WEAVE_VERSION}" -o "$out" ./cmd/weave || exit 1; \
    done

# Node remains available for the existing service health probe. Loom runs in
# the Go service; CLI engines belong only to the optional executor target.
FROM node:22-slim AS server-base
# CA bundle copied from the builder stage — deb.debian.org may be unreachable
# behind proxies/mirrors; the static weave binary still needs it for TLS.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /weave /usr/local/bin/weave
COPY --from=builder /dist/runtime /dist/runtime
ENV WEAVE_RUNTIME_DIST_DIR=/dist/runtime
EXPOSE 8080
ENTRYPOINT ["weave"]

# Explicitly built on execution nodes; glibc supports the native CLI binaries.
FROM server-base AS executor
ARG NPM_REGISTRY
ARG OPENCODE_VERSION
ARG CODEX_VERSION
ARG CLAUDE_CODE_VERSION
RUN --mount=type=cache,target=/root/.npm \
    npm install -g --no-audit --no-fund --registry="${NPM_REGISTRY}" \
      "opencode-ai@${OPENCODE_VERSION}" \
      "@openai/codex@${CODEX_VERSION}" \
      "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}"
# opencode + codex are ready out of the box. claude-code needs a post-install
# native-binary download that can fail offline; make it best-effort so the image
# still includes the other execution engines. A claude
# worker on a network-restricted host runs `claude install` once at deploy time.
RUN node "$(npm root -g)/@anthropic-ai/claude-code/install.cjs" || \
    echo "claude native binary deferred — run 'claude install' at deploy if using the claude engine"

# Keep the server last: plain docker build must not include the executor layer.
FROM server-base AS server
