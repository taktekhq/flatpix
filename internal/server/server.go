// Package server is the HTTP side of Flatpix: it parses a Cloudinary-style
// URL, finds the original, renders (or serves the stored render) and answers.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/taktekhq/flatpix/internal/cache"
	"github.com/taktekhq/flatpix/internal/cldurl"
	"github.com/taktekhq/flatpix/internal/ferr"
	"github.com/taktekhq/flatpix/internal/fetch"
	"github.com/taktekhq/flatpix/internal/gate"
	"github.com/taktekhq/flatpix/internal/group"
	"github.com/taktekhq/flatpix/internal/origin"
	"github.com/taktekhq/flatpix/internal/render"
	"github.com/taktekhq/flatpix/internal/scratch"
	"github.com/taktekhq/flatpix/internal/sniff"
	"github.com/taktekhq/flatpix/internal/transform"
)

// Downloader fetches remote originals. *fetch.Fetcher implements it.
type Downloader interface {
	Check(raw string) (*url.URL, error)
	Download(ctx context.Context, raw string, dst io.Writer, max int64) (int64, error)
}

// Options configure a Server.
type Options struct {
	Origins  *origin.Router
	Fetch    Downloader  // nil turns /image/fetch/ off
	Cache    cache.Store // nil means no derived cache
	Renderer render.Renderer
	Scratch  *scratch.Dir

	Seats     int // concurrent renders
	Downloads int // concurrent downloads

	MaxFileBytes    int64
	MaxInputPixels  int64
	MaxOutputPixels int64

	// Three separate budgets. None covers another.
	QueueTimeout    time.Duration // waiting for a seat or a download slot
	DownloadTimeout time.Duration // fetching one original
	RenderTimeout   time.Duration // rendering one output

	Lenient bool // ignore parameters that cannot be honored
	Logger  *slog.Logger
	Version string
}

// Server implements http.Handler.
type Server struct {
	o         Options
	log       *slog.Logger
	metrics   *Metrics
	seats     *gate.Gate
	downloads *gate.Gate
	group     group.Group[*output]
}

// output is a rendered file in scratch. It is freed when the last requester
// has sent it and the cache upload is done.
type output struct {
	path        string
	size        int64
	contentType string
	format      string
	releaseDisk func()
}

func (o *output) Free() {
	os.Remove(o.path)
	if o.releaseDisk != nil {
		o.releaseDisk()
	}
}

// New builds a Server.
func New(o Options) *Server {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Seats < 1 {
		o.Seats = 1
	}
	if o.Downloads < 1 {
		o.Downloads = 1
	}
	s := &Server{o: o, log: o.Logger, metrics: NewMetrics(), seats: gate.New(int64(o.Seats)), downloads: gate.New(int64(o.Downloads))}
	m := s.metrics
	m.Gauge("flatpix_seats", func() float64 { return float64(o.Seats) })
	m.Gauge("flatpix_seats_in_use", func() float64 { return float64(s.seats.InUse()) })
	m.Gauge("flatpix_queue_depth", func() float64 { return float64(s.seats.Waiting()) })
	m.Gauge("flatpix_downloads_in_use", func() float64 { return float64(s.downloads.InUse()) })
	m.Gauge("flatpix_scratch_budget_bytes", func() float64 { return float64(o.Scratch.Budget()) })
	m.Gauge("flatpix_scratch_used_bytes", func() float64 { return float64(o.Scratch.BudgetUsed()) })
	m.Gauge("flatpix_groups_in_flight", func() float64 { return float64(s.group.Len()) })
	return s
}

// Metrics exposes the counters, for tests and for the binary.
func (s *Server) Metrics() *Metrics { return s.metrics }

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// reqInfo collects what the request log line reports.
type reqInfo struct {
	cloud, cache, format, fetchURL string
}

// ServeHTTP implements http.Handler. It does not use http.ServeMux, which
// would clean the double slash in fetch URLs.
func (s *Server) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	start := time.Now()
	w := &statusWriter{ResponseWriter: rw}
	info := &reqInfo{}
	defer func() { s.logRequest(r, w, info, time.Since(start)) }()

	switch r.URL.Path {
	case "/healthz":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	case "/metrics":
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		s.metrics.WriteText(w)
	case "/":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "Flatpix %s\nPaths look like /{cloud}/image/upload/w_400,c_fill/photo.jpg\n", s.o.Version)
	default:
		s.image(w, r, info)
	}
}

func (s *Server) image(w *statusWriter, r *http.Request, info *reqInfo) {
	defer func() { s.metrics.Inc("flatpix_requests_total", "code", strconv.Itoa(statusOf(w))) }()
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		s.fail(w, r, info, ferr.New(ferr.ErrBadInput, "method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	req, err := cldurl.Parse(r.URL.Path, r.URL.RawQuery)
	if err != nil {
		var pe *cldurl.ParseError
		if errors.As(err, &pe) {
			kind := ferr.ErrBadInput
			if pe.Status == http.StatusNotFound {
				kind = ferr.ErrNotFound
			}
			s.fail(w, r, info, ferr.New(kind, "%s", pe.Msg), 0)
			return
		}
		s.fail(w, r, info, err, 0)
		return
	}
	info.cloud = req.Cloud
	if req.Kind == cldurl.Fetch {
		info.fetchURL = req.FetchURL
	}
	spec, err := transform.Compile(req.Chain, s.o.Lenient)
	if err != nil {
		s.fail(w, r, info, err, 0)
		return
	}
	if len(spec.Ignored) > 0 {
		s.metrics.Add("flatpix_ignored_parameters_total", int64(len(spec.Ignored)))
		w.Header().Set("X-Flatpix-Ignored", strings.Join(spec.Ignored, ","))
	}
	org, ok := s.o.Origins.For(req.Cloud)
	if !ok {
		s.fail(w, r, info, ferr.New(ferr.ErrNotFound, "unknown cloud %q", req.Cloud), 0)
		return
	}

	// Output format: f_ wins; otherwise the URL extension; f_auto negotiates.
	want := spec.Format
	if want == transform.FormatOriginal {
		switch req.Ext {
		case "jpg", "jpeg":
			want = transform.FormatJPG
		case "png", "webp", "avif", "gif":
			want = transform.Format(req.Ext)
		}
	}
	if want == transform.FormatAuto {
		w.Header().Add("Vary", "Accept")
	}
	out := transform.ResolveFormat(want, r.Header.Get("Accept"))
	info.format = string(out)
	fmtToken := string(out)
	if fmtToken == "" {
		fmtToken = "orig"
	}
	spec.Format = out

	// Identity of the source, for the cache key.
	parts := cache.KeyParts{Cloud: req.Cloud, Source: req.Source(), Transform: spec.Normalize(), Format: fmtToken}
	var resolved *origin.Info
	switch {
	case req.Kind == cldurl.Fetch:
		if s.o.Fetch == nil {
			s.fail(w, r, info, ferr.New(ferr.ErrBadInput, "fetching remote images is disabled: set FLATPIX_FETCH_ALLOW to the hosts you trust"), 0)
			return
		}
		if _, err := s.o.Fetch.Check(req.FetchURL); err != nil {
			s.fail(w, r, info, err, 0)
			return
		}
		parts.Identity = "url"
	case req.Version != "":
		parts.Identity = "v:" + req.Version
	default:
		inf, err := origin.Resolve(r.Context(), org, req.PublicID, req.Ext)
		if err != nil {
			s.fail(w, r, info, err, 0)
			return
		}
		if inf.Size > s.o.MaxFileBytes {
			s.fail(w, r, info, ferr.New(ferr.ErrTooLarge, "the original is %d bytes, the limit is %d", inf.Size, s.o.MaxFileBytes), 0)
			return
		}
		resolved = &inf
		parts.ResolvedKey = inf.Key
		parts.Identity = "etag:" + inf.ETag
	}
	key := cache.Key(parts)
	etag := `"` + key[:32] + `"`
	h := w.Header()
	h.Set("ETag", etag)
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Timing-Allow-Origin", "*")
	if req.Version != "" {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "public, max-age=3600")
	}
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		info.cache = "304"
		s.metrics.Inc("flatpix_cache_hits_total", "tier", "client")
		w.WriteHeader(http.StatusNotModified)
		return
	}

	if s.o.Cache != nil {
		entry, err := s.o.Cache.Get(r.Context(), key)
		if err != nil {
			s.log.Warn("cache_get_failed", "err", err.Error())
		} else if entry != nil {
			info.cache = "HIT"
			s.metrics.Inc("flatpix_cache_hits_total", "tier", "derived")
			s.send(w, r, info, entry.Body, entry.Size, entry.ContentType)
			entry.Body.Close()
			return
		}
	}

	work := func(ctx context.Context) (*output, error) {
		return s.renderOnce(ctx, req, org, resolved, spec, out)
	}
	var finish func(ctx context.Context, o *output) error
	if s.o.Cache != nil {
		finish = func(ctx context.Context, o *output) error {
			cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			if err := s.o.Cache.Put(cctx, key, o.path); err != nil {
				s.metrics.Inc("flatpix_cache_put_errors_total")
				return err
			}
			return nil
		}
	}
	lease, shared, err := s.group.Do(r.Context(), key, work, finish)
	if err != nil {
		s.fail(w, r, info, err, 0)
		return
	}
	defer lease.Release()
	if shared {
		info.cache = "SHARED"
		s.metrics.Inc("flatpix_cache_hits_total", "tier", "shared")
	} else {
		info.cache = "MISS"
		s.metrics.Inc("flatpix_cache_misses_total")
	}
	f, err := os.Open(lease.Value.path)
	if err != nil {
		s.fail(w, r, info, ferr.Wrap(ferr.ErrInternal, err, "rendered file missing"), 0)
		return
	}
	defer f.Close()
	s.send(w, r, info, f, lease.Value.size, lease.Value.contentType)
}

// send writes a stored output.
func (s *Server) send(w *statusWriter, r *http.Request, info *reqInfo, body io.Reader, size int64, contentType string) {
	w.Header().Set("X-Flatpix-Cache", info.cache)
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Now().Add(2 * time.Minute)) //nolint:errcheck // not every writer supports it
	w.Header().Set("Content-Type", contentType)
	if rs, ok := body.(io.ReadSeeker); ok {
		http.ServeContent(w, r, "", time.Time{}, rs)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		io.Copy(w, body) //nolint:errcheck // the client may leave mid-way
	}
}

func etagMatches(header, etag string) bool {
	for _, t := range strings.Split(header, ",") {
		t = strings.TrimSpace(t)
		if t == "*" || strings.TrimPrefix(t, "W/") == etag {
			return true
		}
	}
	return false
}

// renderOnce is the work of one group: get the original, take a seat, render.
// ctx stays alive while any requester is connected.
func (s *Server) renderOnce(ctx context.Context, req *cldurl.Request, org origin.Origin, resolved *origin.Info,
	spec *transform.Spec, out transform.Format) (*output, error) {

	// 1. The original, on disk. This happens BEFORE a seat is taken, so a slow
	// download never holds a render slot.
	src, cleanup, err := s.fetchOriginal(ctx, req, org, resolved)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	head, err := readHead(src)
	if err != nil {
		return nil, ferr.Wrap(ferr.ErrInternal, err, "could not read the original")
	}
	format := sniff.Sniff(head)
	if format.Class() == sniff.Rejected {
		return nil, ferr.New(ferr.ErrBadInput, "the original is not a supported image (JPEG, PNG, WebP, AVIF, GIF, TIFF, HEIC)")
	}

	// 2. A seat, waited for under the queue budget.
	qctx, cancel := context.WithTimeout(ctx, s.o.QueueTimeout)
	t0 := time.Now()
	releaseSeat, waited, err := s.seats.AcquireWait(qctx, 1)
	cancel()
	if err != nil {
		return nil, s.busy(ctx, err, "seats")
	}
	defer releaseSeat()
	if waited {
		s.metrics.Inc("flatpix_queue_waits_total")
		s.metrics.Observe("flatpix_queue_wait_seconds", time.Since(t0).Seconds())
	}

	// 3. The render, under its own budget.
	dst, err := s.o.Scratch.NewFile("out-*")
	if err != nil {
		return nil, ferr.Wrap(ferr.ErrInternal, err, "could not create the output file")
	}
	dst.Close()
	rendered := false
	defer func() {
		if !rendered { // an error or a panic: do not leave the file behind
			os.Remove(dst.Name())
		}
	}()
	rctx, cancel := context.WithTimeout(ctx, s.o.RenderTimeout)
	defer cancel()
	t1 := time.Now()
	res, err := s.o.Renderer.Render(rctx, render.Job{
		Src: src, SrcFormat: format, Spec: spec, OutFormat: out, Dst: dst.Name(),
		Limits: render.Limits{MaxInputPixels: s.o.MaxInputPixels, MaxOutputPixels: s.o.MaxOutputPixels},
	})
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			err = ferr.New(ferr.ErrInternal, "the render took longer than %s", s.o.RenderTimeout)
		}
		if ctx.Err() == nil { // a client that left is not a render failure
			s.metrics.Inc("flatpix_render_errors_total", "kind", ferr.Event(err))
		}
		return nil, err
	}
	rendered = true
	s.metrics.Observe("flatpix_render_seconds", time.Since(t1).Seconds())
	s.metrics.Inc("flatpix_renders_total", "format", res.Format)
	return &output{
		path: dst.Name(), size: res.Size, contentType: res.ContentType, format: res.Format,
		releaseDisk: s.o.Scratch.Charge(res.Size),
	}, nil
}

// busy maps a failed wait to a 503, unless the requesters themselves left.
func (s *Server) busy(ctx context.Context, err error, reason string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	s.metrics.Inc("flatpix_rejections_total", "reason", reason)
	return ferr.Wrap(ferr.ErrBusy, err, "the server is busy, try again shortly")
}

// fetchOriginal returns a path to the original and a cleanup function. A
// local file is used in place. Anything else is streamed to a scratch file
// under a download slot and a scratch disk reservation.
func (s *Server) fetchOriginal(ctx context.Context, req *cldurl.Request, org origin.Origin, resolved *origin.Info) (string, func(), error) {
	var key string
	if req.Kind == cldurl.Upload {
		if resolved == nil { // versioned URL: first contact with the origin
			inf, err := origin.Resolve(ctx, org, req.PublicID, req.Ext)
			if err != nil {
				return "", nil, err
			}
			if inf.Size > s.o.MaxFileBytes {
				return "", nil, ferr.New(ferr.ErrTooLarge, "the original is %d bytes, the limit is %d", inf.Size, s.o.MaxFileBytes)
			}
			resolved = &inf
		}
		key = resolved.Key
		if p, ok := org.LocalPath(key); ok {
			return p, func() {}, nil
		}
	}

	qctx, cancel := context.WithTimeout(ctx, s.o.QueueTimeout)
	slot, err := s.downloads.Acquire(qctx, 1)
	cancel()
	if err != nil {
		return "", nil, s.busy(ctx, err, "downloads")
	}
	defer slot()
	qctx, cancel = context.WithTimeout(ctx, s.o.QueueTimeout)
	lease, err := s.o.Scratch.Reserve(qctx, s.o.MaxFileBytes)
	cancel()
	if err != nil {
		return "", nil, s.busy(ctx, err, "scratch")
	}
	f, err := s.o.Scratch.NewFile("src-*")
	if err != nil {
		lease.Release()
		return "", nil, ferr.Wrap(ferr.ErrInternal, err, "could not create the download file")
	}
	fail := func(err error) (string, func(), error) {
		f.Close()
		os.Remove(f.Name())
		lease.Release()
		return "", nil, err
	}
	dctx, cancel := context.WithTimeout(ctx, s.o.DownloadTimeout)
	defer cancel()
	t0 := time.Now()
	var n int64
	if req.Kind == cldurl.Fetch {
		n, err = s.o.Fetch.Download(dctx, req.FetchURL, f, s.o.MaxFileBytes)
	} else {
		n, err = org.Download(dctx, key, f, s.o.MaxFileBytes)
	}
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			err = ferr.New(ferr.ErrUpstream, "the original took longer than %s to download", s.o.DownloadTimeout)
		}
		return fail(err)
	}
	if err := f.Close(); err != nil {
		return fail(ferr.Wrap(ferr.ErrInternal, err, "could not write the download"))
	}
	s.metrics.Observe("flatpix_download_seconds", time.Since(t0).Seconds())
	lease.Shrink(n)
	return f.Name(), func() { os.Remove(f.Name()); lease.Release() }, nil
}

func readHead(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, sniff.HeadSize)
	n, err := io.ReadFull(f, b)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, err
	}
	return b[:n], nil
}

func statusOf(w *statusWriter) int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// fail answers with an error. override forces a status (0 keeps the kind's).
// A request whose client left is not answered, counted or logged as a failure.
func (s *Server) fail(w *statusWriter, r *http.Request, info *reqInfo, err error, override int) {
	if ferr.ClientGone(r.Context(), err) {
		s.metrics.Inc("flatpix_clients_gone_total")
		info.cache = "gone"
		return
	}
	status := ferr.Status(err)
	if override != 0 {
		status = override
	}
	var fe *ferr.Error
	if !errors.As(err, &fe) && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
		// A budget ran out without a kind: that is a failure on our side.
		err = ferr.Wrap(ferr.ErrInternal, err, "timed out")
		status = http.StatusInternalServerError
	}
	switch ferr.Kind(err) {
	case ferr.ErrInternal, ferr.ErrUpstream, ferr.ErrBusy:
		s.log.Warn(ferr.Event(err), "cloud", info.cloud, "path", logPath(r, info), "err", logErr(err))
	}
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Del("ETag")
	if status == http.StatusServiceUnavailable {
		h.Set("Retry-After", "2")
	}
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		fmt.Fprintf(w, "flatpix: %s\n", ferr.PublicMessage(err))
	}
}

// logErr is the error text for logs. govips errors carry a Go stack; keep
// only the first line.
func logErr(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}

// logPath is the request path for logs: no query, no credentials, at most 1 KB.
func logPath(r *http.Request, info *reqInfo) string {
	p := r.URL.Path
	if info.fetchURL != "" {
		if i := strings.Index(p, "/fetch/"); i >= 0 {
			p = p[:i+len("/fetch/")] + fetch.LogURL(info.fetchURL)
		}
	} else if i := strings.Index(p, "/fetch/"); i >= 0 {
		// Not parsed (an error came first): never log what follows /fetch/.
		p = p[:i+len("/fetch/")] + "<url>"
	}
	if len(p) > 1024 {
		p = p[:1024]
	}
	return p
}

func (s *Server) logRequest(r *http.Request, w *statusWriter, info *reqInfo, d time.Duration) {
	if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
		return
	}
	s.log.Info("request", "method", r.Method, "path", logPath(r, info), "status", statusOf(w), "bytes", w.bytes,
		"ms", d.Milliseconds(), "cache", info.cache, "cloud", info.cloud, "format", info.format)
}
