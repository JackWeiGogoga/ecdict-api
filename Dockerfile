FROM golang:1.26-bookworm AS builder
WORKDIR /src

# Servers in mainland China set these via build args (see docker-compose.yml / .env):
# GOPROXY=https://goproxy.cn,direct  GOSUMDB=sum.golang.google.cn  APT_MIRROR=mirrors.aliyun.com
ARG GOPROXY=https://proxy.golang.org,direct
ARG GOSUMDB=sum.golang.org
ENV GOPROXY=${GOPROXY} GOSUMDB=${GOSUMDB}

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# The builder runs on the target platform (CGO is required by go-sqlite3), so GOARCH follows
# TARGETARCH when BuildKit provides it and is native otherwise.
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=1 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -tags "sqlite_fts5" -o /out/api ./cmd/api
RUN CGO_ENABLED=1 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -tags "sqlite_fts5" -o /out/importer ./cmd/importer

FROM debian:bookworm-slim AS runtime
WORKDIR /app

ARG APT_MIRROR=
RUN if [ -n "$APT_MIRROR" ]; then sed -i "s|deb.debian.org|${APT_MIRROR}|g" /etc/apt/sources.list.d/debian.sources; fi \
    && apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/api /usr/local/bin/api
COPY --from=builder /out/importer /usr/local/bin/importer
# The schema is embedded in the binaries; the file is kept for SCHEMA_PATH / -schema overrides.
COPY migrations/schema.sql ./migrations/schema.sql

# Non-root runtime user. A bind-mounted ./data must be writable by uid 10001
# (on the host: sudo chown -R 10001:10001 ./data).
RUN groupadd --system --gid 10001 app \
    && useradd --system --uid 10001 --gid 10001 --no-create-home --shell /usr/sbin/nologin app \
    && mkdir -p /app/data /app/datasets \
    && chown -R 10001:10001 /app/data

ENV HTTP_ADDR=:8080
ENV DB_PATH=/app/data/dict.db

USER 10001
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 CMD ["api", "-healthcheck"]

CMD ["api"]
