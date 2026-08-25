ARG NPM_REGISTRY=https://registry.npmmirror.com
ARG OPENCODE_VERSION=1.18.11
ARG CODEX_VERSION=0.146.0
ARG CLAUDE_CODE_VERSION=2.1.220

FROM node:22-alpine AS ui-builder

ARG NPM_REGISTRY

WORKDIR /build/weave-app

COPY weave-app/package.json weave-app/package-lock.json ./
RUN npm config set registry "${NPM_REGISTRY}" \
    && npm config set replace-registry-host always
RUN --mount=type=cache,target=/root/.npm \
    npm ci --no-audit --no-fund

COPY weave-app/index.html weave-app/tsconfig.json weave-app/tsconfig.app.json weave-app/tsconfig.node.json weave-app/vite.config.ts weave-app/eslint.config.js ./
COPY weave-app/public ./public
COPY weave-app/src ./src
RUN npm run build

FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /build

COPY . .
# Git metadata is excluded by .dockerignore, so the commit must be supplied by
# the host (see `make docker-build`); plain `docker compose build` falls back
# to "unknown".
ARG BUILD_COMMIT=unknown
RUN rm -rf internal/app/webui/dist
COPY --from=ui-builder /build/weave-app/dist ./internal/app/webui/dist
RUN CGO_ENABLED=0 go build -ldflags "-X main.buildCommit=${BUILD_COMMIT}" -o /weave ./cmd/weave
RUN mkdir -p /dist/runtime && \
    for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do \
      os=${target%/*}; arch=${target#*/}; \
      out=/dist/runtime/weave-runtime-$os-$arch; \
      [ "$os" = windows ] && out=$out.exe; \
      CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build \
        -ldflags "-X main.buildCommit=${BUILD_COMMIT}" -o "$out" ./cmd/weave || exit 1; \
    done

# Runtime bundles the third-party CLI engines the workers spawn as subprocesses.
# glibc (node:22-slim) rather than alpine/musl so the native opencode binary runs.
FROM node:22-slim
ARG NPM_REGISTRY
ARG OPENCODE_VERSION
ARG CODEX_VERSION
ARG CLAUDE_CODE_VERSION
# CA bundle copied from the builder stage — deb.debian.org may be unreachable
# behind proxies/mirrors; the static weave binary still needs it for TLS.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
RUN --mount=type=cache,target=/root/.npm \
    npm install -g --no-audit --no-fund --registry="${NPM_REGISTRY}" \
      "opencode-ai@${OPENCODE_VERSION}" \
      "@openai/codex@${CODEX_VERSION}" \
      "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}"
# opencode + codex are ready out of the box. claude-code needs a post-install
# native-binary download that can fail offline; make it best-effort so the image
# always builds with the Demaike-critical engines (opencode) working. A claude
# worker on a network-restricted host runs `claude install` once at deploy time.
RUN node "$(npm root -g)/@anthropic-ai/claude-code/install.cjs" || \
    echo "claude native binary deferred — run 'claude install' at deploy if using the claude engine"
COPY --from=builder /weave /usr/local/bin/weave
COPY --from=builder /dist/runtime /dist/runtime
ENV WEAVE_RUNTIME_DIST_DIR=/dist/runtime
EXPOSE 8080
ENTRYPOINT ["weave"]
