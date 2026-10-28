# Flatpix design

This page explains why Flatpix works the way it does. It is for people who
want to change it. Each section says what the rule is and where it lives.

## Goal

Serve Cloudinary-style image URLs from your own box. A request names an
original and a transformation. Flatpix renders it once, stores the result, and
serves the stored copy from then on.

Non-goals for v0.1: video, signed URL checks, named transformations, a UI, an
upload API. Originals are put in a folder or a bucket by something else.

## The path of a request

```
URL  ->  parse  ->  compile transformation  ->  source identity  ->  cache key
                                                                      |
     client has it (ETag)?  ------------------------------------------+--> 304
     derived cache has it?  ----------------------------------------------> serve
     otherwise, one group per key:
        download original (own gate, own disk budget, own timeout)
        wait for a seat (FIFO, own timeout)
        render (own timeout)
        keep the output until the cache upload is done and the last client is served
```

Code: `internal/server` is the pipeline. `internal/cldurl` parses URLs.
`internal/transform` turns parameters into a normal form and an exact pixel plan.

## Two render paths

Web images (JPEG, PNG, WebP, AVIF, GIF, TIFF, HEIC) render in the server
process with libvips. This is the fast path.

Print files (PDF, AI, EPS, PS, PSD) will render in a child process of the same
binary. The parent starts it by re-executing itself with `FLATPIX_CHILD=render`,
in its own process group. On cancel or timeout the parent sends SIGKILL to the
whole group, so Ghostscript and ImageMagick children die with it. Every tool is
pinned to one thread (`MAGICK_THREAD_LIMIT=1`, `OMP_NUM_THREADS=1`, libvips
concurrency 1), because the seats already bound the parallelism. The child gets
a small environment: `PATH`, `LANG`, and the scratch variables. It never sees
the parent's secrets.

v0.1 ships the seam, not the print renderers:

- `internal/procgroup` starts a process group and kills it. It is tested with a
  grandchild that must die too.
- `internal/render.Child` re-executes the binary and speaks JSON over stdin and
  stdout. `render.RunChild` is the child's entry point. It works for web
  images today: set `FLATPIX_ISOLATE=1` to send every render through it. A test
  checks it gives byte-identical output to the in-process path.
- `render.Dispatcher` picks the path from the sniffed format. A print file
  gets a clear 400 saying print files arrive in v0.2.

## One scratch directory

Every temporary file lives in one directory (`FLATPIX_TMPDIR`): downloaded
originals, rendered outputs, libvips spool files, and the spool files of child
tools (`TMPDIR` and `MAGICK_TMPDIR` point there). At boot the server empties its
contents. It does not remove the directory itself, because that may be a mount
point. A sweeper runs every 2 minutes and removes entries older than 10 minutes.
Code: `internal/scratch`.

## Seats

`FLATPIX_SEATS` is the number of renders that run at once. The seats are a
first come, first served gate (`internal/gate`). A waiter gives up when its
context ends. If a seat is granted at the very instant the context ended, the
seat is handed straight back. A request that waits longer than
`FLATPIX_QUEUE_TIMEOUT` gets `503` with `Retry-After: 2`.

The same gate type is used for download slots and for the scratch disk budget
(weighted by bytes).

## Groups

Requests for the same output share one render (`internal/group`). The key is
the cache key.

- The render runs on its own context. It stays alive while at least one
  requester is connected, and is canceled when the last one has gone.
- The output file is freed only when the last requester has finished sending it.
- The entry stays in the group until the derived cache upload has finished. A
  request that arrives during the upload reuses the local file. It does not
  render the same thing again.
- A panic in a render or in the upload is caught. The key is removed, so later
  requests do not hang on it.

## Download before the seat

A slow download must not hold a render seat. So the original is fetched to
scratch first, under its own limits:

- a download gate (`FLATPIX_DOWNLOADS`),
- a scratch disk budget (`FLATPIX_SCRATCH_MB`). A download reserves the largest
  file it might write, then shrinks the reservation to the real size. A burst
  of distinct large originals waits, and then gets 503, instead of filling the
  disk.

A local folder origin needs no download. The file is read in place.

## Three time budgets

None covers another:

| Budget | Setting | What it bounds |
| --- | --- | --- |
| Queue | `FLATPIX_QUEUE_TIMEOUT` | each wait for a seat, a download slot or disk budget |
| Download | `FLATPIX_DOWNLOAD_TIMEOUT` | fetching one original |
| Render | `FLATPIX_RENDER_TIMEOUT` | rendering one output |

An in-process render is checked between steps. A child process render is killed
outright.

## Fetching remote originals

`/image/fetch/` is the riskiest feature, so it is narrow. Code:
`internal/fetch`.

- https only.
- The host (or host plus path prefix, for one bucket on a shared storage host)
  must be on `FLATPIX_FETCH_ALLOW`. With an empty list, fetch is off.
- No IP literals in any spelling (dotted, decimal, hex, IPv6).
- Private, loopback, link-local, carrier-grade NAT, multicast and reserved
  addresses are refused at connect time, so a DNS answer cannot sneak one in.
  An address inside a NAT64 prefix is checked as the IPv4 address it wraps.
- No redirects. A 3xx answer is an error.
- A URL with credentials is refused. The server never logs a fetch URL's
  credentials, query or fragment, and caps logged URLs at 1 KB.
- The body is read with a cap of `max+1` bytes, so oversize is detected. A
  body of 0 bytes is bad input.
- No proxy is used.

## Format is read from the bytes

`internal/sniff` identifies PNG, JPEG, GIF, RIFF/WebP, TIFF (II and MM),
AVIF and HEIC, PDF, PostScript (the DSC header after an optional BOM and
whitespace), EPS (also the DOS binary header `C5 D0 D3 C6`) and PSD. The file
name and extension are only a hint. A PNG called `photo.jpg` is a PNG. Anything
that is not on the list is refused before libvips sees it, and libvips runs with
`VIPS_BLOCK_UNTRUSTED=1`.

## Errors and logs

Every error that can reach a client has one kind (`internal/ferr`):

| Kind | Status | Log event |
| --- | --- | --- |
| bad input | 400 | `bad_input` |
| not found | 404 | `not_found` |
| too large | 413 | `too_large` |
| busy | 503 | `busy` |
| upstream fetch failed | 502 | `upstream_failed` |
| internal | 500 | `internal_error` |

Bad input and not found are not logged as warnings: the client caused them.
Internal messages are never sent to the client. A request canceled because the
client left is not counted as a failure and is not logged as one. It has its own
counter, `flatpix_clients_gone_total`. Secrets are never logged: the S3 keys
show as set or unset, and the request log has no query string.

## Cache keys

The derived cache key is a SHA-256 over length-prefixed fields:

1. the cloud name,
2. the source path as requested, with its extension (or the remote URL),
3. the stored object that was found, with its extension (empty for versioned URLs),
4. the source version, or its ETag,
5. the normalized transformation,
6. the output format after negotiation.

Two sources that differ only by extension never share a key. The normalized
transformation makes `w_300,h_200,c_fill`, `c_fill,h_200,w_300` and
`h_200,w_300.0,c_fill` the same key.

A versioned URL (`v1234/`) is trusted as the identity of the source. A cache hit
for it never touches the origin. An unversioned URL looks the object up and uses
its ETag, so replacing the file changes the key.

## Sizes from an area budget

When a pixel budget applies and the request did not ask for a size, both
dimensions come from the area and the aspect ratio (`transform.FitArea`). A
10000x1000 image under a 25 megapixel budget stays 10000x1000. A long-edge cap
would have shrunk it to 5000x500. When the request names a size that is over the
budget, it is refused with a 400 instead.

## How raster images are loaded

libvips thumbnail-from-file, never from a buffer, so the original is never held
in memory. Header checks use a lazy streamed reader, so no pixel is decoded to
learn the size, and a header that claims an absurd size is refused as too large
before decoding.

For a big downscale (3x or more) the loader shrinks while it decodes, to twice
the final size (JPEG DCT scaling, WebP scaling). The final resize then runs from
that image. A 4000x3000 JPEG to 800 pixels is decoded at 1/4 size.

For TIFF, the loader's `unlimited` flag is used, so large Photoshop tags do not
fail. libvips 8.17 added the flag, which is why the image builds libvips from
source. If the flag is missing at startup (an older libvips), the server
detects it and runs without it.

## Where the pixel choices came from

The oracle (`cmd/flatpix-oracle`) renders the same URL in Flatpix and in
Cloudinary and scores the pixels. These choices came out of it:

- Rotation comes before resize inside one component. `a_90,w_300` is 300
  pixels wide after the turn.
- The final resample is Mitchell. It agreed better than libvips' default
  Lanczos3 in every case tried.
- Grayscale weights the gamma-encoded channels with Rec. 709. libvips' own
  black and white works in linear light and is much brighter.
- Padding and cropping from the centre round a half pixel up. Fill rounds down.
- JPEG default quality is 90. Default WebP is 75. `q_auto` tiers are best 90,
  good 80, eco 68, low 52.

Content-aware gravity (`g_auto`) uses libvips attention or entropy. It does not
match Cloudinary's model, and the oracle shows it.

## Tests

Test images are generated by the test suite (`internal/testimg`): patterned
photos, split colours, EXIF orientation, headers that claim 60000x60000 pixels,
a PNG named `.jpg`, junk bytes, and the magic bytes of print files. No real
photo or customer file is in the repository.

Everything that needs libvips runs in Docker: `scripts/docker-go.sh test -race ./...`.
