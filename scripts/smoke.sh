#!/bin/sh
# Start a built image, ask it for a resized picture, check the answer, stop it.
# Usage: scripts/smoke.sh flatpix:ci
set -eu
image="${1:?usage: smoke.sh IMAGE}"
cd "$(dirname "$0")/.."
work="$(mktemp -d)"
name="flatpix-smoke-$$"
cleanup() { docker rm -f "$name" >/dev/null 2>&1 || true; rm -rf "$work"; }
trap cleanup EXIT

go run ./cmd/flatpix-bench -gen "$work/photo.jpg" -size 1200x800 >/dev/null
chmod -R a+rX "$work"
port="${PORT:-18099}"
docker run -d --name "$name" -p "$port:8080" -v "$work:/data:ro" "$image" >/dev/null

i=0
until curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1; do
  i=$((i + 1))
  [ "$i" -gt 40 ] && { docker logs "$name"; echo "server did not start"; exit 1; }
  sleep 0.5
done

fetch() { curl -fsS -o "$work/out" -w '%{http_code} %{content_type} %{size_download}\n' "$@"; }

echo "resize:   $(fetch "http://127.0.0.1:$port/demo/image/upload/w_300,c_fill,h_200/photo.jpg")"
case "$(file -b "$work/out")" in
  *"300x200"*) ;;
  *) echo "expected a 300x200 JPEG, got: $(file -b "$work/out")"; exit 1 ;;
esac
echo "webp:     $(fetch -H 'Accept: image/webp' "http://127.0.0.1:$port/demo/image/upload/f_auto,w_300/photo.jpg")"
echo "avif:     $(fetch -H 'Accept: image/avif' "http://127.0.0.1:$port/demo/image/upload/f_auto,w_300/photo.jpg")"
code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/demo/image/upload/e_sepia/photo.jpg")"
[ "$code" = "400" ] || { echo "an unknown parameter should be a 400, got $code"; exit 1; }
curl -fsS "http://127.0.0.1:$port/metrics" | grep -q '^flatpix_renders_total' || { echo "no render metric"; exit 1; }

# The Docker health check runs the binary itself.
i=0
until [ "$(docker inspect -f '{{.State.Health.Status}}' "$name")" = "healthy" ]; do
  i=$((i + 1))
  [ "$i" -gt 40 ] && { echo "container never became healthy"; exit 1; }
  sleep 1
done
[ "$(docker exec "$name" id -u)" != "0" ] || { echo "the container runs as root"; exit 1; }
echo "smoke test passed"
