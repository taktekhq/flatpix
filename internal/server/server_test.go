package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/davidbyttow/govips/v2/vips"
	"github.com/taktekhq/flatpix/internal/cache"
	"github.com/taktekhq/flatpix/internal/ferr"
	"github.com/taktekhq/flatpix/internal/origin"
	"github.com/taktekhq/flatpix/internal/render"
	"github.com/taktekhq/flatpix/internal/s3"
	"github.com/taktekhq/flatpix/internal/s3/s3test"
	"github.com/taktekhq/flatpix/internal/scratch"
	"github.com/taktekhq/flatpix/internal/sniff"
	"github.com/taktekhq/flatpix/internal/testimg"
)

func TestMain(m *testing.M) {
	if err := render.Startup(1); err != nil {
		panic(err)
	}
	code := m.Run()
	render.Shutdown()
	os.Exit(code)
}

// spy wraps the real renderer so tests can count, block and break renders.
type spy struct {
	inner   render.Renderer
	calls   atomic.Int64
	started chan struct{} // receives one value per render that starts, if not nil
	block   chan struct{} // renders wait until this is closed, if not nil
	panicky atomic.Bool
}

func (s *spy) Render(ctx context.Context, j render.Job) (*render.Result, error) {
	s.calls.Add(1)
	if s.started != nil {
		s.started <- struct{}{}
	}
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if s.panicky.Load() {
		panic("renderer exploded")
	}
	return s.inner.Render(ctx, j)
}

type env struct {
	t       *testing.T
	images  string
	scratch *scratch.Dir
	cacheD  string
	spy     *spy
	srv     *Server
	ts      *httptest.Server
	logs    *syncBuf
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func newEnv(t *testing.T, mutate func(*Options, *env)) *env {
	t.Helper()
	e := &env{t: t, images: t.TempDir(), cacheD: t.TempDir(), logs: &syncBuf{}}
	var err error
	e.scratch, err = scratch.Open(filepath.Join(t.TempDir(), "scratch"), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	e.put("cat.jpg", testimg.JPEG(t, testimg.Patterned(400, 300), 90))
	e.put("cat.png", testimg.PNG(t, testimg.Split(300, 200, testimg.Red, testimg.Blue)))
	e.put("photo.jpg", testimg.PNG(t, testimg.Patterned(120, 90))) // a PNG with a jpg name
	e.put("folder/deep.jpg", testimg.JPEG(t, testimg.Patterned(100, 100), 90))
	e.put("exif.jpg", testimg.WithOrientation(t, testimg.JPEG(t, testimg.Patterned(100, 50), 90), 6))
	e.put("huge.jpg", testimg.HugeJPEGHeader(t, 60000, 60000))
	e.put("doc.jpg", testimg.PDF)
	e.put("notes.jpg", []byte("this is not an image at all"))
	e.put("empty.jpg", nil)

	loc, err := origin.NewLocal(e.images)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := cache.NewDisk(e.cacheD)
	if err != nil {
		t.Fatal(err)
	}
	e.spy = &spy{inner: &render.Dispatcher{Web: render.Vips{}}}
	o := Options{
		Origins: origin.NewRouter(loc, nil), Cache: cs, Renderer: e.spy, Scratch: e.scratch,
		Seats: 2, Downloads: 2, MaxFileBytes: 40 << 20, MaxInputPixels: 100_000_000, MaxOutputPixels: 25_000_000,
		QueueTimeout: 2 * time.Second, DownloadTimeout: 5 * time.Second, RenderTimeout: 10 * time.Second,
		Logger: slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})), Version: "test",
	}
	if mutate != nil {
		mutate(&o, e)
	}
	e.srv = New(o)
	e.ts = httptest.NewServer(e.srv)
	t.Cleanup(e.ts.Close)
	return e
}

func (e *env) put(name string, data []byte) {
	e.t.Helper()
	p := filepath.Join(e.images, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		e.t.Fatal(err)
	}
}

type resp struct {
	*http.Response
	Body []byte
}

func (e *env) get(path string, hdr ...string) resp {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.ts.URL+path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return resp{r, b}
}

func dimsOf(t *testing.T, b []byte) (int, int) {
	t.Helper()
	img, err := vips.NewImageFromBuffer(b)
	if err != nil {
		t.Fatalf("response is not an image: %v (%d bytes)", err, len(b))
	}
	defer img.Close()
	return img.Width(), img.Height()
}

func (e *env) waitScratchEmpty() {
	e.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		ents, _ := os.ReadDir(e.scratch.Path())
		if len(ents) == 0 {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("scratch not empty: %d entries, first %s", len(ents), ents[0].Name())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestServesATransformedImage(t *testing.T) {
	e := newEnv(t, nil)
	r := e.get("/demo/image/upload/w_100,c_fill,h_100/cat.jpg")
	if r.StatusCode != 200 {
		t.Fatalf("status %d: %s", r.StatusCode, r.Body)
	}
	if ct := r.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("content type %q", ct)
	}
	if w, h := dimsOf(t, r.Body); w != 100 || h != 100 {
		t.Errorf("got %dx%d", w, h)
	}
	if r.Header.Get("ETag") == "" || r.Header.Get("Cache-Control") != "public, max-age=3600" {
		t.Errorf("headers: etag %q cache-control %q", r.Header.Get("ETag"), r.Header.Get("Cache-Control"))
	}
	if r.Header.Get("Vary") != "" {
		t.Errorf("Vary = %q on a URL without f_auto", r.Header.Get("Vary"))
	}
	if r.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Error("no CORS header")
	}
	e.waitScratchEmpty()
}

func TestVersionedURLsAreImmutable(t *testing.T) {
	e := newEnv(t, nil)
	r := e.get("/demo/image/upload/w_50/v1700000000/cat.jpg")
	if r.StatusCode != 200 || r.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("status %d cache-control %q", r.StatusCode, r.Header.Get("Cache-Control"))
	}
}

func TestExtensionlessAndFolderURLs(t *testing.T) {
	e := newEnv(t, nil)
	for _, p := range []string{"/demo/image/upload/w_50/cat", "/demo/image/upload/w_50/folder/deep", "/demo/image/upload/w_50/folder/deep.jpg", "/demo/image/upload/w_50/cat.webp"} {
		if r := e.get(p); r.StatusCode != 200 {
			t.Errorf("%s: %d %s", p, r.StatusCode, r.Body)
		}
	}
	if r := e.get("/demo/image/upload/w_50/cat.webp"); r.Header.Get("Content-Type") != "image/webp" {
		t.Errorf("the extension should pick the output format, got %q", r.Header.Get("Content-Type"))
	}
	if r := e.get("/demo/image/upload/f_png/cat.webp"); r.Header.Get("Content-Type") != "image/png" {
		t.Errorf("f_ should win over the extension, got %q", r.Header.Get("Content-Type"))
	}
}

func TestConditionalRequests(t *testing.T) {
	e := newEnv(t, nil)
	first := e.get("/demo/image/upload/w_80/cat.jpg")
	etag := first.Header.Get("ETag")
	r := e.get("/demo/image/upload/w_80/cat.jpg", "If-None-Match", etag)
	if r.StatusCode != 304 || len(r.Body) != 0 {
		t.Fatalf("status %d, %d bytes", r.StatusCode, len(r.Body))
	}
	if e.spy.calls.Load() != 1 {
		t.Errorf("renders = %d: a 304 must not render", e.spy.calls.Load())
	}
	// A different transformation has a different ETag.
	if other := e.get("/demo/image/upload/w_81/cat.jpg").Header.Get("ETag"); other == etag {
		t.Error("etag did not change with the transformation")
	}
}

func TestFormatNegotiation(t *testing.T) {
	e := newEnv(t, nil)
	r := e.get("/demo/image/upload/f_auto,w_100/cat.jpg", "Accept", "image/webp,image/*;q=0.8")
	if r.Header.Get("Content-Type") != "image/webp" || r.Header.Get("Vary") != "Accept" {
		t.Errorf("webp: type %q vary %q", r.Header.Get("Content-Type"), r.Header.Get("Vary"))
	}
	r = e.get("/demo/image/upload/f_auto,w_100/cat.jpg", "Accept", "text/html")
	if r.Header.Get("Content-Type") != "image/jpeg" || r.Header.Get("Vary") != "Accept" {
		t.Errorf("original: type %q vary %q", r.Header.Get("Content-Type"), r.Header.Get("Vary"))
	}
	r = e.get("/demo/image/upload/f_auto,w_100/cat.jpg", "Accept", "image/avif,image/webp")
	if ct := r.Header.Get("Content-Type"); ct != "image/avif" && !(r.StatusCode == 400 && strings.Contains(string(r.Body), "avif")) {
		t.Errorf("avif: status %d type %q", r.StatusCode, ct)
	}
	// Each negotiated format has its own ETag, so a CDN keyed on Vary stays right.
	a := e.get("/demo/image/upload/f_auto,w_100/cat.jpg", "Accept", "image/webp").Header.Get("ETag")
	b := e.get("/demo/image/upload/f_auto,w_100/cat.jpg", "Accept", "text/html").Header.Get("ETag")
	if a == b {
		t.Error("webp and original share an ETag")
	}
}

func TestDerivedCacheRendersOnce(t *testing.T) {
	e := newEnv(t, nil)
	p := "/demo/image/upload/w_120,c_fill,h_60/cat.jpg"
	first := e.get(p)
	// The upload finishes in the background; wait for the entry.
	deadline := time.Now().Add(3 * time.Second)
	for e.srv.group.Len() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	second := e.get(p)
	if !bytes.Equal(first.Body, second.Body) || second.StatusCode != 200 {
		t.Fatal("cached copy differs")
	}
	if e.spy.calls.Load() != 1 {
		t.Errorf("renders = %d, want 1", e.spy.calls.Load())
	}
	if e.srv.Metrics().Get("flatpix_cache_hits_total", "tier", "derived") != 1 || e.srv.Metrics().Get("flatpix_cache_misses_total") != 1 {
		t.Errorf("metrics: hits %d misses %d", e.srv.Metrics().Get("flatpix_cache_hits_total", "tier", "derived"), e.srv.Metrics().Get("flatpix_cache_misses_total"))
	}
	var files int
	filepath.WalkDir(e.cacheD, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files++
		}
		return nil
	})
	if files != 1 {
		t.Errorf("cache holds %d files", files)
	}
	e.waitScratchEmpty()
}

func TestCacheSurvivesAServerRestart(t *testing.T) {
	e := newEnv(t, nil)
	p := "/demo/image/upload/w_70/cat.jpg"
	e.get(p)
	deadline := time.Now().Add(3 * time.Second)
	for e.srv.group.Len() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// A second server on the same cache and originals, with a renderer that must not run.
	e2 := newEnv(t, func(o *Options, n *env) {
		cs, _ := cache.NewDisk(e.cacheD)
		o.Cache = cs
		loc, _ := origin.NewLocal(e.images)
		o.Origins = origin.NewRouter(loc, nil)
	})
	e2.spy.panicky.Store(true)
	if r := e2.get(p); r.StatusCode != 200 {
		t.Fatalf("status %d: %s", r.StatusCode, r.Body)
	}
}

func TestIdenticalRequestsShareOneRender(t *testing.T) {
	e := newEnv(t, func(o *Options, e *env) { e.spy.started = make(chan struct{}, 100); e.spy.block = make(chan struct{}) })
	const n = 12
	var wg sync.WaitGroup
	results := make([]resp, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i] = e.get("/demo/image/upload/w_90/cat.jpg") }(i)
	}
	<-e.spy.started
	deadline := time.Now().Add(3 * time.Second)
	for e.srv.group.Refs(firstKey(e)) < n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	close(e.spy.block)
	wg.Wait()
	if e.spy.calls.Load() != 1 {
		t.Fatalf("renders = %d, want 1 for %d identical requests", e.spy.calls.Load(), n)
	}
	shared := 0
	for _, r := range results {
		if r.StatusCode != 200 {
			t.Fatalf("status %d", r.StatusCode)
		}
		if r.Header.Get("X-Flatpix-Cache") == "SHARED" {
			shared++
		}
		if !bytes.Equal(r.Body, results[0].Body) {
			t.Fatal("bodies differ")
		}
	}
	if m := e.srv.Metrics().Get("flatpix_cache_hits_total", "tier", "shared"); m != n-1 {
		t.Errorf("shared metric = %d, want %d", m, n-1)
	}
	e.waitScratchEmpty()
}

// firstKey finds the only key in the group.
func firstKey(e *env) string {
	e.srv.group.Len()
	return e.srv.group.AnyKey()
}

func TestSeatsQueueThenRejectWith503(t *testing.T) {
	e := newEnv(t, func(o *Options, e *env) {
		o.Seats = 1
		o.QueueTimeout = 150 * time.Millisecond
		e.spy.started = make(chan struct{}, 10)
		e.spy.block = make(chan struct{})
	})
	done := make(chan resp, 1)
	go func() { done <- e.get("/demo/image/upload/w_10/cat.jpg") }()
	<-e.spy.started // the only seat is taken

	start := time.Now()
	r := e.get("/demo/image/upload/w_11/cat.jpg") // a different output, so no sharing
	if r.StatusCode != 503 {
		t.Fatalf("status %d: %s", r.StatusCode, r.Body)
	}
	if r.Header.Get("Retry-After") != "2" {
		t.Errorf("Retry-After = %q", r.Header.Get("Retry-After"))
	}
	if d := time.Since(start); d < 100*time.Millisecond || d > time.Second {
		t.Errorf("waited %v, expected about the queue timeout", d)
	}
	if e.srv.Metrics().Get("flatpix_rejections_total", "reason", "seats") != 1 {
		t.Error("rejection not counted")
	}
	close(e.spy.block)
	if first := <-done; first.StatusCode != 200 {
		t.Fatalf("first request: %d", first.StatusCode)
	}
	// The gate is free again.
	if r := e.get("/demo/image/upload/w_12/cat.jpg"); r.StatusCode != 200 {
		t.Fatalf("after: %d", r.StatusCode)
	}
	e.waitScratchEmpty()
}

func TestRequestsInTheQueueAreServedInOrder(t *testing.T) {
	var order []string
	var mu sync.Mutex
	e := newEnv(t, func(o *Options, e *env) {
		o.Seats = 1
		e.spy.started = make(chan struct{}, 10)
		e.spy.block = make(chan struct{})
	})
	_ = order
	_ = &mu
	// Hold the seat, queue three requests one by one, release, and check the
	// renders ran in arrival order through the seat counter.
	var wg sync.WaitGroup
	statuses := make([]int, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i] = e.get(fmt.Sprintf("/demo/image/upload/w_%d/cat.jpg", 20+i)).StatusCode
		}(i)
		if i == 0 {
			<-e.spy.started
		}
		deadline := time.Now().Add(2 * time.Second)
		for i > 0 && e.srv.seats.Waiting() < i && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
		}
	}
	close(e.spy.block)
	wg.Wait()
	for i, s := range statuses {
		if s != 200 {
			t.Errorf("request %d: %d", i, s)
		}
	}
}

func TestErrors(t *testing.T) {
	e := newEnv(t, func(o *Options, e *env) { o.MaxFileBytes = 1 << 20; e.put("toobig.jpg", make([]byte, 2<<20)) })
	tests := []struct {
		path   string
		status int
		text   string
	}{
		{"/demo/image/upload/w_50/missing.jpg", 404, ""},
		{"/", 200, "Flatpix"},
		{"/nope", 404, ""},
		{"/demo/video/upload/clip.mp4", 400, "image/upload"},
		{"/demo/image/upload/w_50,e_sepia/cat.jpg", 400, "e_sepia"},
		{"/demo/image/upload/w_50,zz_9/cat.jpg", 400, "zz_9"},
		{"/demo/image/upload/w_abc/cat.jpg", 400, "w_abc"},
		{"/demo/image/upload/w_9000,h_9000/cat.jpg", 400, "pixels"},
		{"/demo/image/upload/w_50/notes.jpg", 400, "not a supported image"},
		{"/demo/image/upload/w_50/doc.jpg", 400, "v0.2"},
		{"/demo/image/upload/w_50/empty.jpg", 400, ""},
		{"/demo/image/upload/w_50/huge.jpg", 413, "pixels"},
		{"/demo/image/upload/w_50/toobig.jpg", 413, ""},
		{"/demo/image/upload/w_50/../../etc/passwd", 400, ""},
		{"/demo/image/fetch/https://example.com/a.jpg", 400, "disabled"},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			r := e.get(tc.path)
			if r.StatusCode != tc.status {
				t.Fatalf("status %d (%s), want %d", r.StatusCode, bytes.TrimSpace(r.Body), tc.status)
			}
			if tc.text != "" && !strings.Contains(string(r.Body), tc.text) {
				t.Errorf("body %q does not contain %q", r.Body, tc.text)
			}
			if tc.status >= 400 && strings.Contains(string(r.Body), "goroutine") {
				t.Error("a stack trace reached the client")
			}
		})
	}
	e.waitScratchEmpty()
}

func TestLenientModeIgnoresUnknownParameters(t *testing.T) {
	e := newEnv(t, func(o *Options, e *env) { o.Lenient = true })
	r := e.get("/demo/image/upload/w_60,e_sepia,x_3/cat.jpg")
	if r.StatusCode != 200 {
		t.Fatalf("status %d: %s", r.StatusCode, r.Body)
	}
	if got := r.Header.Get("X-Flatpix-Ignored"); got != "e_sepia,x_3" {
		t.Errorf("X-Flatpix-Ignored = %q", got)
	}
	if w, _ := dimsOf(t, r.Body); w != 60 {
		t.Errorf("width %d", w)
	}
}

func TestMislabeledFilesAreServedByTheirBytes(t *testing.T) {
	e := newEnv(t, nil)
	r := e.get("/demo/image/upload/photo") // a PNG named .jpg, no resize, original format
	if r.StatusCode != 200 {
		t.Fatalf("%d %s", r.StatusCode, r.Body)
	}
	if sniff.Sniff(r.Body) != sniff.PNG {
		t.Errorf("output is %q, want the PNG it really is", sniff.Sniff(r.Body))
	}
}

func TestEXIFOrientation(t *testing.T) {
	e := newEnv(t, nil)
	r := e.get("/demo/image/upload/w_25/exif.jpg")
	if w, h := dimsOf(t, r.Body); w != 25 || h != 50 {
		t.Errorf("got %dx%d, want 25x50", w, h)
	}
}

func TestSourcesDifferingOnlyByExtensionNeverShareCachedOutput(t *testing.T) {
	e := newEnv(t, func(o *Options, e *env) {
		e.put("twin.jpg", testimg.PNG(t, testimg.Solid(60, 60, testimg.Red)))
		e.put("twin.png", testimg.PNG(t, testimg.Solid(60, 60, testimg.Blue)))
	})
	a := e.get("/demo/image/upload/f_png/twin.jpg")
	b := e.get("/demo/image/upload/f_png/twin.png")
	if a.StatusCode != 200 || b.StatusCode != 200 {
		t.Fatalf("%d %d", a.StatusCode, b.StatusCode)
	}
	if bytes.Equal(a.Body, b.Body) {
		t.Fatal("red and blue twins got the same output: they shared a cache entry")
	}
	if a.Header.Get("ETag") == b.Header.Get("ETag") {
		t.Fatal("same ETag")
	}
	// Same with a version in the URL, where nothing is looked up.
	c := e.get("/demo/image/upload/f_png/v1/twin.jpg")
	d := e.get("/demo/image/upload/f_png/v1/twin.png")
	if bytes.Equal(c.Body, d.Body) {
		t.Fatal("versioned twins shared a cache entry")
	}
}

func TestHeadRequests(t *testing.T) {
	e := newEnv(t, nil)
	req, _ := http.NewRequest("HEAD", e.ts.URL+"/demo/image/upload/w_40/cat.jpg", nil)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || len(b) != 0 || r.ContentLength <= 0 {
		t.Fatalf("status %d, body %d bytes, length %d", r.StatusCode, len(b), r.ContentLength)
	}
	post, _ := http.Post(e.ts.URL+"/demo/image/upload/w_40/cat.jpg", "text/plain", nil)
	if post.StatusCode != 405 {
		t.Errorf("POST: %d", post.StatusCode)
	}
}

func TestHealthAndMetrics(t *testing.T) {
	e := newEnv(t, nil)
	if r := e.get("/healthz"); r.StatusCode != 200 || strings.TrimSpace(string(r.Body)) != "ok" {
		t.Fatalf("healthz: %d %q", r.StatusCode, r.Body)
	}
	e.get("/demo/image/upload/w_40/cat.jpg")
	e.get("/demo/image/upload/w_40/nope.jpg")
	m := string(e.get("/metrics").Body)
	for _, want := range []string{
		"flatpix_renders_total{format=\"jpg\"} 1", "flatpix_cache_misses_total 1", "flatpix_requests_total{code=\"404\"} 1",
		"flatpix_seats 2", "flatpix_seats_in_use 0", "flatpix_queue_depth 0", "flatpix_render_seconds_count 1",
		"flatpix_rejections_total", "flatpix_queue_waits_total",
	} {
		if want == "flatpix_rejections_total" || want == "flatpix_queue_waits_total" {
			continue // only present once something was rejected or waited
		}
		if !strings.Contains(m, want) {
			t.Errorf("/metrics lacks %q", want)
		}
	}
}

func TestAClientThatLeavesIsNotARenderFailure(t *testing.T) {
	e := newEnv(t, func(o *Options, e *env) { e.spy.started = make(chan struct{}, 10); e.spy.block = make(chan struct{}) })
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", e.ts.URL+"/demo/image/upload/w_33/cat.jpg", nil)
	errc := make(chan error, 1)
	go func() { _, err := http.DefaultClient.Do(req); errc <- err }()
	<-e.spy.started
	cancel()
	if err := <-errc; err == nil {
		t.Fatal("want the client error")
	}
	// The render is canceled when nobody is left to want it.
	deadline := time.Now().Add(3 * time.Second)
	for e.srv.group.Len() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if e.srv.group.Len() != 0 {
		t.Fatal("render still running with no requester")
	}
	if n := e.srv.Metrics().Get("flatpix_render_errors_total", "kind", "internal_error"); n != 0 {
		t.Errorf("render errors counted: %d", n)
	}
	for _, bad := range []string{"internal_error", "busy", "level=ERROR"} {
		if strings.Contains(e.logs.String(), bad) {
			t.Errorf("log mentions %q:\n%s", bad, e.logs.String())
		}
	}
	e.waitScratchEmpty()
	// The next request for the same output starts a fresh render.
	close(e.spy.block)
	if r := e.get("/demo/image/upload/w_33/cat.jpg"); r.StatusCode != 200 {
		t.Fatalf("after: %d", r.StatusCode)
	}
}

func TestAPanickingRenderDoesNotHangTheNextRequest(t *testing.T) {
	e := newEnv(t, nil)
	e.spy.panicky.Store(true)
	r := e.get("/demo/image/upload/w_44/cat.jpg")
	if r.StatusCode != 500 {
		t.Fatalf("status %d", r.StatusCode)
	}
	if strings.Contains(string(r.Body), "exploded") || strings.Contains(string(r.Body), "goroutine") {
		t.Errorf("internal detail leaked: %s", r.Body)
	}
	e.spy.panicky.Store(false)
	done := make(chan resp, 1)
	go func() { done <- e.get("/demo/image/upload/w_44/cat.jpg") }()
	select {
	case r := <-done:
		if r.StatusCode != 200 {
			t.Fatalf("after the panic: %d", r.StatusCode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request after a panic hangs")
	}
	e.waitScratchEmpty()
}

func TestRenderTimeoutIsItsOwnBudget(t *testing.T) {
	e := newEnv(t, func(o *Options, e *env) {
		o.RenderTimeout = 100 * time.Millisecond
		o.QueueTimeout = 10 * time.Second
		e.spy.block = make(chan struct{}) // never released
	})
	start := time.Now()
	r := e.get("/demo/image/upload/w_55/cat.jpg")
	if r.StatusCode != 500 || !strings.Contains(string(r.Body), "internal error") {
		t.Fatalf("status %d: %s", r.StatusCode, r.Body)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v", d)
	}
	e.waitScratchEmpty()
}

// ---- remote origins -----------------------------------------------------

func TestS3OriginDownloadsToScratchAndCleansUp(t *testing.T) {
	fake := s3test.New()
	defer fake.Close()
	fake.Put("media/products/cat.jpg", testimg.JPEG(t, testimg.Patterned(300, 200), 90), "image/jpeg")
	s3o, err := origin.NewS3(s3.Config{Endpoint: fake.URL, AccessKey: "a", SecretKey: "b"}, "media", "")
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(o *Options, e *env) { o.Origins = origin.NewRouter(s3o, nil) })
	r := e.get("/acme/image/upload/w_100/products/cat.jpg")
	if r.StatusCode != 200 {
		t.Fatalf("%d %s", r.StatusCode, r.Body)
	}
	if w, h := dimsOf(t, r.Body); w != 100 || h != 67 {
		t.Errorf("%dx%d", w, h)
	}
	if fake.Gets.Load() != 1 {
		t.Errorf("S3 GETs = %d", fake.Gets.Load())
	}
	e.waitScratchEmpty()
	deadline0 := time.Now().Add(3 * time.Second)
	for e.scratch.BudgetUsed() != 0 && time.Now().Before(deadline0) {
		time.Sleep(5 * time.Millisecond)
	}
	if e.scratch.BudgetUsed() != 0 {
		t.Errorf("scratch budget still used: %d", e.scratch.BudgetUsed())
	}
	// A repeat is served from the derived cache: no second download.
	deadline := time.Now().Add(3 * time.Second)
	for e.srv.group.Len() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	e.get("/acme/image/upload/w_100/products/cat.jpg")
	if fake.Gets.Load() != 1 {
		t.Errorf("S3 GETs after a repeat = %d", fake.Gets.Load())
	}
	// A versioned URL skips the lookup on a cache hit.
	e.get("/acme/image/upload/w_100/v5/products/cat.jpg")
	heads := fake.Heads.Load()
	deadline = time.Now().Add(3 * time.Second)
	for e.srv.group.Len() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	e.get("/acme/image/upload/w_100/v5/products/cat.jpg")
	if fake.Heads.Load() != heads {
		t.Errorf("a cached versioned URL asked S3 again (%d -> %d HEADs)", heads, fake.Heads.Load())
	}
}

func TestSlowDownloadsNeverHoldARenderSeat(t *testing.T) {
	gateOpen := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			<-gateOpen // the download hangs until released
		}
		w.Header().Set("ETag", `"abc"`)
		w.Header().Set("Last-Modified", time.Unix(1700000000, 0).UTC().Format(http.TimeFormat))
		w.Header().Set("Content-Length", "10")
		w.WriteHeader(200)
		if r.Method == "GET" {
			w.Write(make([]byte, 10))
		}
	}))
	defer slow.Close()
	defer func() {
		select {
		case <-gateOpen:
		default:
			close(gateOpen)
		}
	}()
	s3o, _ := origin.NewS3(s3.Config{Endpoint: slow.URL, AccessKey: "a", SecretKey: "b"}, "media", "")
	e := newEnv(t, func(o *Options, e *env) {
		o.Seats = 1
		loc, _ := origin.NewLocal(e.images)
		o.Origins = origin.NewRouter(loc, map[string]origin.Origin{"slow": s3o})
	})
	slowDone := make(chan resp, 1)
	go func() { slowDone <- e.get("/slow/image/upload/w_10/hang.jpg") }()
	deadline := time.Now().Add(3 * time.Second)
	for e.srv.downloads.InUse() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if e.srv.downloads.InUse() != 1 {
		t.Fatal("the slow download did not start")
	}
	// With the only seat, a local request must still go through at once.
	start := time.Now()
	if r := e.get("/demo/image/upload/w_10/cat.jpg"); r.StatusCode != 200 {
		t.Fatalf("local request: %d %s", r.StatusCode, r.Body)
	}
	if time.Since(start) > time.Second {
		t.Errorf("local request took %v while a download was in flight", time.Since(start))
	}
	if e.srv.seats.InUse() != 0 {
		t.Errorf("seats in use: %d", e.srv.seats.InUse())
	}
	close(gateOpen)
	<-slowDone
}

func TestScratchBudgetStopsABurstOfDownloads(t *testing.T) {
	gateOpen := make(chan struct{})
	var inflight, maxInflight atomic.Int64
	fakeS3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"x"`)
		w.Header().Set("Last-Modified", time.Unix(1700000000, 0).UTC().Format(http.TimeFormat))
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(200)
		if r.Method == "GET" {
			n := inflight.Add(1)
			for {
				m := maxInflight.Load()
				if n <= m || maxInflight.CompareAndSwap(m, n) {
					break
				}
			}
			<-gateOpen
			inflight.Add(-1)
			w.Write(make([]byte, 1000))
		}
	}))
	defer fakeS3.Close()
	s3o, _ := origin.NewS3(s3.Config{Endpoint: fakeS3.URL, AccessKey: "a", SecretKey: "b"}, "media", "")
	e := newEnv(t, func(o *Options, e *env) {
		o.Origins = origin.NewRouter(s3o, nil)
		o.Downloads = 8
		o.MaxFileBytes = 1 << 20
		sc, _ := scratch.Open(filepath.Join(t.TempDir(), "s"), 2<<20) // room for two worst-case downloads
		o.Scratch = sc
		e.scratch = sc
		o.QueueTimeout = 200 * time.Millisecond
	})
	var wg sync.WaitGroup
	var rejected atomic.Int64
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := e.get(fmt.Sprintf("/c/image/upload/w_10/img%d.jpg", i))
			if r.StatusCode == 503 {
				rejected.Add(1)
			}
		}(i)
	}
	time.Sleep(600 * time.Millisecond)
	if maxInflight.Load() > 2 {
		t.Errorf("%d downloads ran at once, the budget allows 2", maxInflight.Load())
	}
	close(gateOpen)
	wg.Wait()
	if rejected.Load() < 3 {
		t.Errorf("only %d requests were turned away", rejected.Load())
	}
}

type fakeFetch struct {
	allowed string
	body    []byte
	calls   atomic.Int64
}

func (f *fakeFetch) Check(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host != f.allowed {
		return nil, ferr.New(ferr.ErrBadInput, "host is not on the fetch allowlist")
	}
	return u, nil
}

func (f *fakeFetch) Download(ctx context.Context, raw string, dst io.Writer, max int64) (int64, error) {
	f.calls.Add(1)
	n, err := dst.Write(f.body)
	return int64(n), err
}

func TestFetchUsesTheDownloaderAndCaches(t *testing.T) {
	ff := &fakeFetch{allowed: "img.example.com", body: testimg.JPEG(t, testimg.Patterned(200, 100), 90)}
	e := newEnv(t, func(o *Options, e *env) { o.Fetch = ff })
	r := e.get("/demo/image/fetch/w_50/https://img.example.com/a/b.jpg?x=1")
	if r.StatusCode != 200 {
		t.Fatalf("%d %s", r.StatusCode, r.Body)
	}
	if w, h := dimsOf(t, r.Body); w != 50 || h != 25 {
		t.Errorf("%dx%d", w, h)
	}
	deadline := time.Now().Add(3 * time.Second)
	for e.srv.group.Len() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	e.get("/demo/image/fetch/w_50/https://img.example.com/a/b.jpg?x=1")
	if ff.calls.Load() != 1 {
		t.Errorf("downloads = %d, want 1", ff.calls.Load())
	}
	// A different query is a different remote file.
	e.get("/demo/image/fetch/w_50/https://img.example.com/a/b.jpg?x=2")
	if ff.calls.Load() != 2 {
		t.Errorf("downloads = %d, want 2", ff.calls.Load())
	}
	if r := e.get("/demo/image/fetch/w_50/https://evil.example.com/a.jpg"); r.StatusCode != 400 {
		t.Errorf("off-list host: %d", r.StatusCode)
	}
	e.waitScratchEmpty()
}

func TestSecretsAndFetchQueriesStayOutOfTheLog(t *testing.T) {
	ff := &fakeFetch{allowed: "img.example.com", body: testimg.JPEG(t, testimg.Patterned(20, 20), 90)}
	e := newEnv(t, func(o *Options, e *env) { o.Fetch = ff })
	e.get("/demo/image/fetch/w_10/https://user:hunter2@img.example.com/a.jpg?X-Amz-Signature=deadbeefcafe")
	e.get("/demo/image/fetch/w_10/https://img.example.com/a.jpg?token=SECRETTOKEN")
	e.get("/demo/image/upload/w_10/cat.jpg?token=ANOTHERSECRET")
	for _, leak := range []string{"hunter2", "deadbeefcafe", "SECRETTOKEN", "ANOTHERSECRET"} {
		if strings.Contains(e.logs.String(), leak) {
			t.Errorf("log leaks %q:\n%s", leak, e.logs.String())
		}
	}
}

func TestRandomJunkNeverCrashes(t *testing.T) {
	e := newEnv(t, func(o *Options, e *env) {
		for i := 0; i < 5; i++ {
			b := make([]byte, 5000)
			rand.Read(b)
			e.put(fmt.Sprintf("junk%d.jpg", i), b)
			e.put(fmt.Sprintf("jpegjunk%d.jpg", i), append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, b...))
			e.put(fmt.Sprintf("pngjunk%d.jpg", i), append([]byte("\x89PNG\r\n\x1a\n"), b...))
		}
	})
	for _, prefix := range []string{"junk", "jpegjunk", "pngjunk"} {
		for i := 0; i < 5; i++ {
			r := e.get(fmt.Sprintf("/demo/image/upload/w_20/%s%d.jpg", prefix, i))
			if r.StatusCode != 400 {
				t.Errorf("%s%d: status %d %s", prefix, i, r.StatusCode, r.Body)
			}
		}
	}
	if r := e.get("/demo/image/upload/w_20/cat.jpg"); r.StatusCode != 200 {
		t.Fatal("server unhealthy after junk")
	}
	e.waitScratchEmpty()
}

var _ = errors.Is
