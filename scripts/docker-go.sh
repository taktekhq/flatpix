#!/bin/sh
# Run any `go` command inside the dev image, which has libvips.
# Needs only Docker. Examples:
#   scripts/docker-go.sh test -race ./...
#   scripts/docker-go.sh vet ./...
set -eu
cd "$(dirname "$0")/.."
docker build --target dev -t flatpix-dev -q . >/dev/null
exec docker run --rm \
  -v "$PWD":/src \
  -v flatpix-gomod:/go/pkg/mod \
  -v flatpix-gocache:/root/.cache/go-build \
  -w /src \
  -e GOFLAGS="${GOFLAGS:-}" \
  flatpix-dev go "$@"
