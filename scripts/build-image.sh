#!/usr/bin/env bash
# Builds gogoga-dictionary:<git sha> for linux/amd64 on a dev machine (Apple Silicon or x86):
# Set BUILD_APT_MIRROR / BUILD_GOPROXY for mainland-China networks.
# Go is cross-compiled natively with a cross gcc (CGO for go-sqlite3), then only the slim runtime
# image is assembled for amd64. Output: dist/gogoga-dictionary-<sha>.tar.gz for `docker load`.
set -euo pipefail
cd "$(dirname "$0")/.."

TAG="$(git rev-parse --short HEAD)"
# The Go toolchain runs natively (arm64 on Apple Silicon); only the output targets amd64.
GO_IMAGE="golang:1.26-bookworm"
mkdir -p bin/linux-amd64 dist

[ "${SKIP_COMPILE:-}" = 1 ] || docker run --rm --platform "linux/$(uname -m | sed s/x86_64/amd64/ | sed s/aarch64/arm64/)" \
  -v "$PWD":/src -w /src \
  -v gogoga-gomod:/go/pkg/mod -v gogoga-gocache:/root/.cache/go-build \
  -e APT_MIRROR="${BUILD_APT_MIRROR:-}" -e GOPROXY="${BUILD_GOPROXY:-https://proxy.golang.org,direct}" \
  "$GO_IMAGE" bash -euc '
    if [ -n "$APT_MIRROR" ]; then sed -i "s|deb.debian.org|$APT_MIRROR|g" /etc/apt/sources.list.d/debian.sources; fi
    apt-get update -qq && apt-get install -y -qq gcc-x86-64-linux-gnu >/dev/null
    export CGO_ENABLED=1 GOOS=linux GOARCH=amd64 CC=x86_64-linux-gnu-gcc
    go build -trimpath -ldflags="-s -w" -tags sqlite_fts5 -o bin/linux-amd64/api ./cmd/api
    go build -trimpath -ldflags="-s -w" -tags sqlite_fts5 -o bin/linux-amd64/importer ./cmd/importer
  '

# .dockerignore excludes bin/, so package from a minimal context of just what the image needs.
CTX="$(mktemp -d)"
trap 'rm -rf "$CTX"' EXIT
mkdir -p "$CTX/bin/linux-amd64" "$CTX/migrations"
cp bin/linux-amd64/api bin/linux-amd64/importer "$CTX/bin/linux-amd64/"
cp migrations/schema.sql "$CTX/migrations/"
cp Dockerfile.prebuilt "$CTX/Dockerfile"
docker build --platform linux/amd64 --build-arg APT_MIRROR="${BUILD_APT_MIRROR:-}" \
  -t "gogoga-dictionary:$TAG" -t gogoga-dictionary:latest "$CTX"
docker save "gogoga-dictionary:$TAG" | gzip > "dist/gogoga-dictionary-$TAG.tar.gz"
echo "dist/gogoga-dictionary-$TAG.tar.gz"
