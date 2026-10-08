FROM node:24.19.0-bookworm-slim@sha256:a9f5f7c91a432850b2a8a7797adf5eadb6c733ceed61167806cee7ea7fbc29df AS node_tools
FROM oven/bun:1.3.11@sha256:0733e50325078969732ebe3b15ce4c4be5082f18c4ac1a0f0ca4839c2e4e42a7 AS frontend-builder
USER root
COPY --from=node_tools /usr/local/ /usr/local/
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates libstdc++6 libatomic1 && rm -rf /var/lib/apt/lists/*
WORKDIR /source
COPY . .
ARG COMMIT
ENV VITE_OPENPOST_REVISION=${COMMIT}
ENV VITE_OPENPOST_ENVIRONMENT=production
RUN bun install --frozen-lockfile
RUN bun run build -- frontend

FROM golang:1.26.6-alpine@sha256:3889b425f035be855a72fb4755265311293b6d414521f0a519d819df32222d83 AS backend-builder
ARG VERSION=saivaree
ARG COMMIT=unknown
RUN apk add --no-cache gcc musl-dev sqlite-dev
WORKDIR /app
COPY apps/server/go.mod apps/server/go.sum ./apps/server/
RUN --mount=type=cache,target=/go/pkg/mod cd apps/server && go mod download
COPY apps/server/ ./apps/server/
COPY --from=frontend-builder /source/apps/server/cmd/openpost/public ./apps/server/cmd/openpost/public
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build cd apps/server && CGO_ENABLED=1 GOOS=linux go build -buildvcs=false -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" -o openpost ./cmd/openpost

FROM alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
ARG VERSION=saivaree
ARG COMMIT=unknown
LABEL org.opencontainers.image.title="OpenPost Saivaree" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.source="https://github.com/siboUNce/openpost"
RUN apk add --no-cache ca-certificates ffmpeg tzdata sqlite 'libcrypto3>=3.5.8-r0' 'libssl3>=3.5.8-r0'
RUN adduser -D -u 1000 openpost
WORKDIR /app
COPY --from=backend-builder /app/apps/server/openpost .
RUN mkdir -p /data/media /data/db && chown -R openpost:openpost /data
USER openpost
ENV OPENPOST_PORT=8080
ENV OPENPOST_EDITION=selfhost
ENV OPENPOST_DATABASE_DRIVER=sqlite
ENV OPENPOST_DATABASE_PATH=/data/db/openpost.db
ENV OPENPOST_STORAGE_DRIVER=local
ENV OPENPOST_MEDIA_PATH=/data/media
ENV OPENPOST_DIAGNOSTICS_STATE_FILE=/data/diagnostics-installation-id
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 CMD wget --no-verbose --tries=1 --spider http://localhost:8080/api/v1/health || exit 1
STOPSIGNAL SIGTERM
CMD ["./openpost","all"]
