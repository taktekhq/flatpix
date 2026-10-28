# syntax=docker/dockerfile:1.7

# ---- dev: Go plus libvips headers. Used to build and to run the tests. ----
ARG GO_IMAGE=golang:1.27-trixie
ARG RUNTIME_IMAGE=debian:trixie-slim
FROM ${GO_IMAGE} AS dev
RUN apt-get update \
 && apt-get install -y --no-install-recommends libvips-dev pkg-config gcc \
 && rm -rf /var/lib/apt/lists/*
# AVIF needs an encoder plugin, HEIC a decoder plugin; Debian ships them apart.
RUN apt-get update \
 && apt-get install -y --no-install-recommends libheif-plugin-aomenc libheif-plugin-dav1d libheif-plugin-libde265 \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /src

# ---- build: compile the server ----
FROM dev AS build
ARG VERSION=dev
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/flatpix ./cmd/flatpix

# ---- runtime: Debian slim with the libvips runtime, non-root ----
FROM ${RUNTIME_IMAGE} AS runtime
RUN apt-get update \
 && apt-get install -y --no-install-recommends libvips42t64 libheif-plugin-aomenc libheif-plugin-dav1d libheif-plugin-libde265 ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --uid 10001 --no-create-home --shell /usr/sbin/nologin flatpix \
 && mkdir -p /data /cache /tmp/flatpix \
 && chown flatpix /cache /tmp/flatpix
COPY --from=build /out/flatpix /usr/local/bin/flatpix
ENV FLATPIX_LISTEN=:8080 \
    FLATPIX_ORIGIN=/data \
    FLATPIX_TMPDIR=/tmp/flatpix \
    VIPS_BLOCK_UNTRUSTED=1 \
    MALLOC_ARENA_MAX=2
USER flatpix
EXPOSE 8080
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD ["/usr/local/bin/flatpix", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/flatpix"]
