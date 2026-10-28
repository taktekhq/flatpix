# Flatpix

An open-source image server that reads Cloudinary URLs. Self-host it for free,
or use the hosted service at <https://flatpix.io>.

Put your originals in a folder or a bucket. Point your existing Cloudinary URLs
at Flatpix. Each transformed image is rendered once, stored, and served from
storage after that.

```
https://img.example.com/demo/image/upload/c_fill,w_400,h_300,f_auto/v1/products/shoe.jpg
```

Flatpix is written in Go and uses libvips. It ships as one Docker image.

## Quick start

Put some pictures in a folder, then run:

```sh
mkdir images && cp ~/Pictures/cat.jpg images/
docker run --rm -p 8080:8080 -v "$PWD/images:/data:ro" ghcr.io/taktekhq/flatpix
```

Open these in a browser:

```
http://localhost:8080/demo/image/upload/cat.jpg
http://localhost:8080/demo/image/upload/w_300,c_fill,h_300/cat.jpg
http://localhost:8080/demo/image/upload/w_300,f_auto/cat.jpg
```

The first part of the path (`demo`) is the cloud name. Any name works. It is
part of the cache key, and you can map different names to different origins.

Add a derived cache so renders survive a restart:

```sh
mkdir -p cache
docker run --rm -p 8080:8080 --user "$(id -u)" \
  -v "$PWD/images:/data:ro" -v "$PWD/cache:/cache" \
  -e FLATPIX_CACHE=/cache ghcr.io/taktekhq/flatpix
```

The image runs as a non-root user. `--user` makes the container write to your
cache folder as you.

Check that it is alive: `curl localhost:8080/healthz`. Look at the counters:
`curl localhost:8080/metrics`.

## URLs

```
/{cloud}/image/upload/[{transformation}/...][v{version}/]{public_id}[.{ext}]
/{cloud}/image/fetch/[{transformation}/...]{url}
```

- A transformation is parameters joined by commas: `w_400,h_300,c_fill`.
- Several transformations joined by slashes run in order, each on the result of
  the one before: `c_fill,w_800,h_800/c_scale,w_400`.
- `f_` and `q_` apply to the whole chain.
- The public id is the path of the file under the origin. Cloudinary public ids
  have no extension, so Flatpix looks for the file as named, then as the bare id,
  then with `.jpg`, `.jpeg`, `.png`, `.webp`, `.gif`, `.avif`, `.tif`, `.tiff`,
  `.heic`.
- The extension in the URL sets the output format when there is no `f_`.
  `cat.webp` gives a WebP, even if the original is a JPEG.
- A version segment (`v1234`) says this content never changes. The response is
  cached for a year, and a cache hit never touches the origin.
- If a folder name looks like a parameter (`us_east`), put a version segment
  before it, as with Cloudinary: `/demo/image/upload/v1/us_east/pic.jpg`.
- A signature segment (`s--abc12345--`) is read and ignored. Flatpix does not
  check signatures yet.
- The query string of an upload URL is ignored. The `_a` analytics parameter that
  Cloudinary SDKs add is dropped from fetch URLs.

### Supported parameters

Anything else is a `400` that names the parameter, for example
`unsupported transformation parameter "e_sepia": the only supported effect is e_grayscale`.
Set `FLATPIX_LENIENT=1` to ignore such parameters instead. The response then
lists them in an `X-Flatpix-Ignored` header.

| Parameter | Values | What it does |
| --- | --- | --- |
| `w_`, `h_` | whole pixels, or a decimal up to 1.0 | Width and height. `0.5` means half of the original. A decimal above 1 is rounded to pixels. Up to 65535. |
| `c_` | `scale` (default), `fit`, `limit`, `mfit`, `fill`, `lfill`, `pad`, `lpad`, `crop`, `thumb` | How to fit the size. See below. |
| `g_` | `center` (default), `north`, `south`, `east`, `west`, `north_east`, `north_west`, `south_east`, `south_west`, `auto`, `auto:entropy` | Which part to keep when cropping or padding. `auto` uses libvips attention, `auto:entropy` uses entropy. |
| `ar_` | `16:9` or a decimal such as `1.5` | Aspect ratio. With `w_` or `h_` it sets the other side. With both it is ignored. Alone, it takes the biggest box with that ratio that fits the original. |
| `dpr_` | a number above 0, at most 10 | Multiplies `w_` and `h_`. |
| `f_` | `auto`, `jpg`, `png`, `webp`, `avif`, `gif` | Output format. `auto` picks AVIF, then WebP, then the original format, from the `Accept` header, and adds `Vary: Accept`. A GIF is its first frame. |
| `q_` | `1` to `100`, `auto`, `auto:best`, `auto:good`, `auto:eco`, `auto:low` | Quality. Default is 90 for JPEG and 75 for WebP. The auto tiers are about 90, 80, 68 and 52. AVIF is set lower for the same look. |
| `b_` | a colour name, or `rgb:RRGGBB`, `rgb:RRGGBBAA`, `rgb:RGB`, `rgb:RGBA` | Background for padding, and for the transparent parts of a PNG saved as JPEG. Without it, padding is transparent where the format allows it, and white otherwise. |
| `e_` | `grayscale` | Black and white. |
| `a_` | `90`, `180`, `270`, `-90`, `-180`, `-270`, `auto`, `exif` | Rotation, clockwise. It runs before the resize, so `a_90,w_300` is 300 pixels wide after the turn. EXIF orientation is always applied, so `auto` and `exif` change nothing. |

Colour names: white, black, red, green, blue, yellow, cyan, aqua, magenta,
fuchsia, gray, grey, silver, orange, purple, pink, brown, navy, teal, lime,
maroon, olive, transparent.

Crop modes:

| `c_` | Result |
| --- | --- |
| `scale` | Resize to `w_` x `h_`, and distort if the ratio differs. With one side, keep the ratio. May enlarge. |
| `fit` | The whole image inside the box, ratio kept. May enlarge. |
| `limit` | Like `fit`, never enlarges. |
| `mfit` | Like `fit`, but only enlarges. A bigger image is left alone. |
| `fill` | Cover the box, keep the ratio, crop the rest by `g_`. May enlarge. |
| `lfill` | Like `fill`, never enlarges. If the image is smaller, it is only cropped to the box. |
| `thumb` | Same as `fill` in Flatpix. |
| `pad` | Fit inside the box, then pad to the exact size with `b_`, placed by `g_`. May enlarge. |
| `lpad` | Like `pad`, never enlarges. |
| `crop` | Cut a `w_` x `h_` window out of the original with no scaling, placed by `g_`. |

Not supported yet: `x_`, `y_`, `r_` (rounded corners), `l_` and `u_` (overlays),
`t_` (named transformations), `$variables`, `if_`, `fl_`, face and object
gravity, `dpr_auto`, `w_auto`, other `e_` effects, signed URL checks, video.

### Fetch

`/demo/image/fetch/w_300/https://images.example.com/a.jpg` downloads a remote
image and serves it. It is off until you set `FLATPIX_FETCH_ALLOW`. The rules
are strict: https only, hosts from your list only, no IP addresses, no private
networks, no redirects, no credentials in the URL. See [docs/DESIGN.md](docs/DESIGN.md).

Entries in the list are `host`, `*.domain`, `host:port`, or `host/prefix` for one
bucket on a shared storage host, such as `storage.googleapis.com/my-bucket`.
Fetch URLs are cached by URL. Change the URL to refresh.

## Origins

An origin is where the originals live. Set `FLATPIX_ORIGIN` to:

- a folder: `/data` or `dir:/data`
- a bucket: `s3://bucket` or `s3://bucket/prefix`

Buckets work with S3-compatible services: AWS S3, Cloudflare R2, MinIO, Google
Cloud Storage (interoperability mode with HMAC keys). Flatpix talks to them with
the [minio-go](https://github.com/minio/minio-go) client. The test suite runs the
S3 origin and the S3 cache against an in-memory S3 server. We have not yet run
them against AWS, R2, MinIO or GCS ourselves, so tell us what you find.

```sh
docker run --rm -p 8080:8080 \
  -e FLATPIX_ORIGIN=s3://my-media/originals \
  -e FLATPIX_S3_ENDPOINT=https://ACCOUNT.r2.cloudflarestorage.com \
  -e FLATPIX_S3_REGION=auto \
  -e FLATPIX_S3_ACCESS_KEY=... -e FLATPIX_S3_SECRET_KEY=... \
  -e FLATPIX_CACHE=s3://my-media/derived \
  ghcr.io/taktekhq/flatpix
```

To give each cloud name its own origin, use `FLATPIX_CLOUDS`:
`FLATPIX_CLOUDS=shop=s3://shop-media,blog=/data/blog`. A name that is not listed
uses `FLATPIX_ORIGIN`.

The derived cache can be a folder or a bucket (`FLATPIX_CACHE`). Nothing in it is
ever deleted by Flatpix. The key covers everything that changes the output, so
old entries are safe to remove with `find` or a bucket lifecycle rule.

## Configuration

Everything is an environment variable. A few have a flag, and the flag wins.

| Variable | Flag | Default | Meaning |
| --- | --- | --- | --- |
| `FLATPIX_LISTEN` | `-listen` | `:8080` | Address to listen on. |
| `FLATPIX_ORIGIN` | `-origin` | `./images` (`/data` in the image) | Default origin. |
| `FLATPIX_CLOUDS` | | | `name=origin,name=origin`. |
| `FLATPIX_CACHE` | `-cache` | none | Derived cache, a folder or `s3://bucket/prefix`. |
| `FLATPIX_SEATS` | `-seats` | number of CPUs | Renders that run at once. |
| `FLATPIX_QUEUE_TIMEOUT` | `-queue-timeout` | `10s` | How long a request waits for a seat before it gets 503. |
| `FLATPIX_DOWNLOADS` | | 2 x seats | Downloads that run at once. |
| `FLATPIX_DOWNLOAD_TIMEOUT` | | `30s` | Time to fetch one original. |
| `FLATPIX_RENDER_TIMEOUT` | | `60s` | Time to render one output. |
| `FLATPIX_MAX_FILE_MB` | `-max-file-mb` | `40` | Largest original. |
| `FLATPIX_MAX_PIXELS` | `-max-pixels` | `25000000` | Most pixels in an output. |
| `FLATPIX_MAX_INPUT_PIXELS` | | `100000000` | Most pixels in an original, read from its header. |
| `FLATPIX_S3_ENDPOINT` | | AWS | Endpoint URL, such as `http://minio:9000`. |
| `FLATPIX_S3_REGION` | | `us-east-1` | Region. R2 uses `auto`. |
| `FLATPIX_S3_ACCESS_KEY`, `FLATPIX_S3_SECRET_KEY` | | | Keys. `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` and `AWS_REGION` work too. Without keys, public objects are read. |
| `FLATPIX_FETCH_ALLOW` | | none | Comma-separated hosts that `/image/fetch/` may reach. |
| `FLATPIX_LENIENT` | `-lenient` | off | `1` ignores parameters Flatpix cannot honor instead of answering 400. |
| `FLATPIX_TMPDIR` | `-tmpdir` | `/tmp/flatpix` | The scratch directory. Emptied at boot. |
| `FLATPIX_SCRATCH_MB` | | `2048` | Disk budget for downloads in the scratch directory. |
| `FLATPIX_VIPS_CONCURRENCY` | | CPUs / seats | Threads one render may use. |
| `FLATPIX_ISOLATE` | | off | `1` renders in child processes (see the design notes). |
| `FLATPIX_LOG` | | text | `json` for JSON logs. |
| `FLATPIX_SHUTDOWN_TIMEOUT` | | `30s` | How long to wait for requests on shutdown. |

`/healthz` answers `ok`. `/metrics` is Prometheus text. It lists renders,
cache hits, queue waits and rejections, and gauges for seats and the queue.
Protect `/metrics` at your proxy if the port is public.

## Headers you get

- `Cache-Control: public, max-age=31536000, immutable` for versioned URLs,
  `public, max-age=3600` for the rest.
- `ETag`, and `304` for `If-None-Match`. The tag comes from the cache key, so a
  `304` costs no render.
- `Vary: Accept` when the URL uses `f_auto`.
- `X-Flatpix-Cache`: `HIT` (stored copy), `MISS` (rendered now) or `SHARED`
  (joined a render already running).

## Design notes

In plain words. The long version is in [docs/DESIGN.md](docs/DESIGN.md).

- **Seats.** There are a fixed number of render seats. The others wait in line,
  first come first served. After `FLATPIX_QUEUE_TIMEOUT` a waiter gets `503` with
  `Retry-After: 2`. A client that leaves gives up its place.
- **Groups.** Ten requests for the same output cause one render. The render
  keeps going while any of them is connected, and stops when all have left.
- **Stored forever.** A render is stored in the derived cache and served from
  there after that. The key has the cloud, the source path, the source version
  or ETag, the transformation and the format.
- **Limits.** Maximum file size, maximum output pixels, maximum input pixels.
  The input size is read from the header, so a file that claims 60000 x 60000
  pixels is refused without decoding it.
- **Shrink before decode.** libvips shrinks while it decodes a big JPEG or WebP,
  so a 4000 x 3000 photo becoming 800 pixels is decoded at a quarter of its size.
- **Download to disk.** Originals from a bucket or a URL are streamed to a temp
  file. They are never held whole in memory. The download happens before a seat
  is taken, with its own limits, so a slow download never blocks a render.
- **Engine.** libvips, through [govips](https://github.com/davidbyttow/govips).
  libvips is the fast, low-memory choice for this job: it streams, it shrinks
  on load, and it is built for many requests at once. govips is an actively
  released Go binding (v2.20.0 came out in October 2026). It gives us thumbnail
  loading from a file, streaming input and a choice of resize kernel. The other
  common binding, bimg, works from in-memory buffers, which does not fit
  "never hold the original in memory". The image builds libvips 8.18 from source
  with only the formats Flatpix needs, so ImageMagick, PDF and SVG loaders are not
  in the process.
- **Print files.** PDF, AI, EPS, PS and PSD are planned for v0.2. They will
  render in a child process killed as a group on timeout. The seam is in the code
  today.

## Does it match Cloudinary?

Flatpix checks itself against Cloudinary. `flatpix-oracle` fetches the original
and Cloudinary's rendering from Cloudinary's public `demo` cloud at run time,
renders the same URL with Flatpix, and scores the pixels. The score is 1 minus
the mean absolute difference over the red, green and blue channels, as a
fraction of 255. 1.0 is identical. The target is 0.988. Nothing fetched is stored
in this repository.

```sh
scripts/docker-go.sh run ./cmd/flatpix-oracle            # needs Docker and internet
scripts/docker-go.sh run ./cmd/flatpix-oracle -markdown  # as a table
```

Result against Cloudinary's `demo` cloud, original `sample.jpg` (864 x 576),
libvips 8.18.7, 31 transformations. All 31 output sizes match Cloudinary's exactly.

| transformation | Cloudinary | Flatpix | score | |
|---|---|---|---|---|
| `w_400` | 400x267 | 400x267 | 0.9906 | ok |
| `w_150` | 150x100 | 150x100 | 0.9818 | below target |
| `h_300` | 450x300 | 450x300 | 0.9915 | ok |
| `w_300,h_300` | 300x300 | 300x300 | 0.9900 | ok |
| `w_0.5` | 432x288 | 432x288 | 0.9895 | ok |
| `dpr_2.0,w_200` | 400x267 | 400x267 | 0.9906 | ok |
| `c_fit,w_300,h_300` | 300x200 | 300x200 | 0.9887 | ok |
| `c_limit,w_3000,h_3000` | 864x576 | 864x576 | 0.9957 | ok |
| `c_mfit,w_3000,h_3000` | 3000x2000 | 3000x2000 | 0.9938 | ok |
| `c_fill,w_300,h_300` | 300x300 | 300x300 | 0.9939 | ok |
| `c_fill,w_300,h_300,g_north` | 300x300 | 300x300 | 0.9862 | below target |
| `c_fill,w_300,h_300,g_south_east` | 300x300 | 300x300 | 0.9939 | ok |
| `c_fill,w_300,h_300,g_auto` | 300x300 | 300x300 | 0.7042 | below target |
| `c_lfill,w_3000,h_300` | 864x300 | 864x300 | 0.9900 | ok |
| `c_thumb,w_200,h_200` | 200x200 | 200x200 | 0.9915 | ok |
| `c_pad,w_400,h_400,b_white` | 400x400 | 400x400 | 0.9935 | ok |
| `c_pad,w_400,h_400,b_rgb:3355aa` | 400x400 | 400x400 | 0.9876 | below target |
| `c_lpad,w_3000,h_3000,b_black` | 3000x3000 | 3000x3000 | 0.9995 | ok |
| `c_crop,w_400,h_300` | 400x300 | 400x300 | 0.9928 | ok |
| `c_crop,w_400,h_300,g_east` | 400x300 | 400x300 | 0.9905 | ok |
| `c_fill,w_301,h_300` | 301x300 | 301x300 | 0.9832 | below target |
| `c_crop,w_401,h_300` | 401x300 | 401x300 | 0.9887 | ok |
| `ar_16:9,c_fill,w_600` | 600x338 | 600x338 | 0.9810 | below target |
| `e_grayscale,w_400` | 400x267 | 400x267 | 0.9936 | ok |
| `a_90,w_300` | 300x450 | 300x450 | 0.9916 | ok |
| `w_400/a_270` | 267x400 | 267x400 | 0.9855 | below target |
| `c_fill,w_300,h_300/c_scale,w_150` | 150x150 | 150x150 | 0.9789 | below target |
| `f_png,w_300` | 300x200 | 300x200 | 0.9966 | ok |
| `f_webp,w_300` | 300x200 | 300x200 | 0.9875 | below target |
| `q_30,w_400` | 400x267 | 400x267 | 0.9771 | below target |
| `q_auto,w_400` | 400x267 | 400x267 | 0.9828 | below target |

**31 compared, min 0.7042, median 0.9900, mean 0.9801, 20 at or above 0.988** The median is above the 0.988 target.

Reading the table:

- Scores under the target are mostly JPEG and WebP noise. Both servers compress
  with their own settings, and a re-encode of the same pixels already scores about
  0.995. The low `q_` rows are the clearest case.
- `g_auto` scores 0.70. Cloudinary picks the crop with its own model. Flatpix
  asks libvips for attention. They choose different windows of the picture.
- `c_fill,w_300,h_300,g_north` scores lower than the same crop from the centre.
  The two crops have the same window on this landscape picture, so the cause is
  in how Cloudinary renders them. We did not find it.
- A chain resamples twice, so a chain loses a little to the order of operations.
- Sizes always match. If a size ever does not, the table says `(resized)`.

The score has limits. It is a pixel average, so it does not see a missing
effect or a different crop if the picture is mostly flat. Use it to catch
regressions and to choose between two ways of doing something, not as proof of
a match.

## Benchmarks

Test: one 4000 x 3000 JPEG (4.3 MB, made by `flatpix-bench -gen`), asked for as
`c_fill,w_800,h_600,f_webp`, with `FLATPIX_SEATS=4`, no derived cache. Each
request changes `q_`, so every request is a real render and nothing is shared.
The image runs in Docker. The client is `flatpix-bench`.

```sh
go run ./cmd/flatpix-bench -gen images/photo.jpg
docker run -d --name fp -p 8080:8080 -e FLATPIX_SEATS=4 -v "$PWD/images:/data:ro" ghcr.io/taktekhq/flatpix
go run ./cmd/flatpix-bench -url http://localhost:8080 -n 800 -c 16
docker exec fp grep VmHWM /proc/1/status      # peak memory
```

Measured on a 12-thread Linux machine with 31 GB of RAM. **The machine was busy
with other jobs: its load average was 35 to 60 during the runs.** Treat the
speed numbers as a floor. A quiet machine will do better.

| Clients | Renders per second | p50 | p95 | p99 | Peak memory |
| --- | --- | --- | --- | --- | --- |
| 4 (every request gets a seat) | 6.8 | 559 ms | 975 ms | 1150 ms | 150 MB |
| 16 (12 of them wait for a seat) | 8.2 | 1802 ms | 3106 ms | 3406 ms | 151 MB |

With 16 clients the latency includes the wait in the queue. All 1200 requests
returned 200. The CPU cost of one render was 0.27 CPU-seconds, read from the
container's cgroup. Most of that is the WebP encoder. The same render to JPEG
costs about half.

Served from the derived cache (a tmpfs, 16 clients, one URL, 4000 requests):
**6,466 requests per second**, p50 2 ms, p95 6 ms, one render in total.

The image is 192 MB.

What made the render cheap: a JPEG is decoded at a quarter of its size
(1000 x 750 here) because the decoder can scale while it works. In our tests,
decoding at half size (2000 x 1500) cost twice the CPU of decoding at a quarter.

## Limitations

- Print files (PDF, AI, EPS, PS, PSD) are not supported yet.
- Animated GIFs are served as their first frame.
- `g_auto` uses libvips attention. It does not pick the same crop as Cloudinary.
- JPEG and WebP outputs from Cloudinary and Flatpix differ in compression
  noise, so even a perfect match scores a little under 1.0 (about 0.995).
- Signed URLs are not checked. A signature segment is ignored.
- No named transformations, overlays, rounded corners or effects other than
  grayscale.
- Nothing deletes old entries from the derived cache.
- An unversioned URL on a bucket origin costs one HEAD request per request,
  to learn the ETag. Put a CDN in front, or use versioned URLs.
- If a decode fails, the Go binding reads the file into memory once to try again.
  That is bounded by `FLATPIX_MAX_FILE_MB`.
- The Docker image is for linux/amd64.
- The S3 origin and cache have only been tested against an in-memory S3 server.
- `/metrics` has no authentication.

## Roadmap

- v0.2: print files (PDF, AI, EPS, PS by Ghostscript, PSD by ImageMagick, first
  page only) in killable child processes.
- Named transformations.
- Signed URLs.
- Multi-tenant: a key and a quota per cloud name.
- A source cache for originals fetched from far away.
- arm64 images.

## Contributing

Issues and pull requests are welcome. Run the tests in Docker, which has libvips:

```sh
scripts/docker-go.sh vet ./...
scripts/docker-go.sh test -race ./...
```

To build the image yourself: `docker build -t flatpix .`. It compiles libvips 8.18
first, which takes a few minutes the first time. To build the binary without
Docker you need Go and the libvips 8.14+ development files (`pkg-config vips`
must work). TIFF input uses the `unlimited` loader flag when libvips has it
(8.17 and later).

Packages that do not touch libvips (`cldurl`, `transform`, `gate`, `group`,
`fetch`, `origin`, `cache`, `config`) also test on the host with plain `go test`.

Add a test with a change. If you change how a transformation renders, run the
oracle and say what happened to the scores.

## About

Built by Nizar Mahmoud, the engineer who moved Sticker Mule off Cloudinary.

Flatpix is made by [Taktek](https://github.com/taktekhq).

## License

AGPL-3.0, copyright Taktek and Nizar Mahmoud. See [LICENSE](LICENSE). The AGPL
covers the Flatpix code. If you modify it and run it as a network service, you
must offer your changes to the people who use that service.
