# syntax=docker/dockerfile:1.7
#
# Stages
#   dev      Go, plus libvips built from source. Used to build and to run the tests.
#   build    compiles the server
#   runtime  Debian slim with only the libraries libvips needs, non-root
#
# libvips is built here, with only the loaders and savers Flatpix uses, instead
# of taking Debian's package. That keeps ImageMagick, PDF, SVG and OpenEXR out of
# the process, shrinks the image, and gives a libvips that has the TIFF loader's
# "unlimited" flag (8.17 and later), which Debian 13's 8.16 lacks.

ARG GO_IMAGE=golang:1.27-trixie
ARG RUNTIME_IMAGE=debian:trixie-slim
ARG VIPS_VERSION=8.18.7

# ---- dev ----
FROM ${GO_IMAGE} AS dev
ARG VIPS_VERSION
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      meson ninja-build pkg-config curl xz-utils ca-certificates \
      libglib2.0-dev libexpat1-dev libjpeg62-turbo-dev libpng-dev libwebp-dev libtiff-dev \
      libheif-dev libexif-dev liblcms2-dev libcgif-dev libimagequant-dev libhwy-dev \
      libheif-plugin-aomenc libheif-plugin-dav1d libheif-plugin-libde265 \
 && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL "https://github.com/libvips/libvips/releases/download/v${VIPS_VERSION}/vips-${VIPS_VERSION}.tar.xz" \
      | tar xJ -C /tmp \
 && cd "/tmp/vips-${VIPS_VERSION}" \
 && meson setup build --prefix=/usr/local --libdir=lib --buildtype=release \
      -Dauto_features=disabled -Dmodules=disabled -Dintrospection=disabled \
      -Dexamples=false -Dcplusplus=false -Ddocs=false \
      -Dppm=false -Danalyze=false -Dradiance=false \
      -Djpeg=enabled -Dpng=enabled -Dwebp=enabled -Dtiff=enabled -Dheif=enabled \
      -Dexif=enabled -Dlcms=enabled -Dcgif=enabled -Dimagequant=enabled -Dhighway=enabled \
 && ninja -C build install \
 && ldconfig \
 && rm -rf /tmp/vips-*
# Which Debian packages this libvips needs at run time.
RUN ldd /usr/local/lib/libvips.so.42 | awk '/=> \//{print $3}' | xargs -r realpath \
      | xargs -r dpkg -S 2>/dev/null | cut -d: -f1 | sort -u > /vips-runtime-pkgs.txt \
 && cat /vips-runtime-pkgs.txt
WORKDIR /src

# ---- build ----
FROM dev AS build
ARG VERSION=dev
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/flatpix ./cmd/flatpix

# ---- runtime ----
FROM ${RUNTIME_IMAGE} AS runtime
COPY --from=dev /vips-runtime-pkgs.txt /tmp/vips-runtime-pkgs.txt
# AVIF needs an encoder plugin and HEIC a decoder plugin; Debian ships them apart.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      $(cat /tmp/vips-runtime-pkgs.txt) \
      libheif-plugin-aomenc libheif-plugin-dav1d libheif-plugin-libde265 ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/* /tmp/vips-runtime-pkgs.txt \
 && useradd --system --uid 10001 --no-create-home --shell /usr/sbin/nologin flatpix \
 && mkdir -p /data /cache /tmp/flatpix \
 && chown flatpix /cache /tmp/flatpix
COPY --from=dev /usr/local/lib/libvips.so* /usr/local/lib/
RUN ldconfig
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
