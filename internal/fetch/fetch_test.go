package fetch

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/taktekhq/flatpix/internal/ferr"
)

func TestCheckPolicy(t *testing.T) {
	f, err := New(Options{Allow: []string{
		"allowed.example.com", "*.cdn.example.com", "storage.example.com/my-bucket", "alt.example.com:8443",
		"127.0.0.1", // an IP literal on the list must still be refused
	}})
	if err != nil {
		t.Fatal(err)
	}
	ok := []string{
		"https://allowed.example.com/a.jpg",
		"https://ALLOWED.example.com/a.jpg",
		"https://allowed.example.com./a.jpg",
		"https://allowed.example.com:443/a.jpg",
		"https://img.cdn.example.com/a/b.png?x=1",
		"https://a.b.cdn.example.com/a.png",
		"https://storage.example.com/my-bucket/a.jpg",
		"https://storage.example.com/my-bucket",
		"https://alt.example.com:8443/a.jpg",
	}
	for _, u := range ok {
		if _, err := f.Check(u); err != nil {
			t.Errorf("Check(%q): %v", u, err)
		}
	}
	bad := map[string]string{
		"http://allowed.example.com/a.jpg":                     "https",
		"https://other.example.com/a.jpg":                      "allow",
		"https://cdn.example.com/a.jpg":                        "allow",
		"https://evilcdn.example.com/a.jpg":                    "allow",
		"https://allowed.example.com.evil.com/a.jpg":           "allow",
		"https://user:pw@allowed.example.com/a.jpg":            "credentials",
		"https://allowed.example.com@evil.com/a.jpg":           "credentials",
		"https://storage.example.com/my-bucket2/a.jpg":         "allow",
		"https://storage.example.com/other/a.jpg":              "allow",
		"https://storage.example.com/my-bucket/../other/a.jpg": "invalid",
		"https://allowed.example.com:8443/a.jpg":               "allow",
		"https://allowed.example.com:22/a.jpg":                 "allow",
		"https://127.0.0.1/a.jpg":                              "ip",
		"https://[::1]/a.jpg":                                  "ip",
		"https://10.0.0.5/a.jpg":                               "ip",
		"https://169.254.169.254/latest/meta-data":             "ip",
		"https://8.8.8.8/a.jpg":                                "ip",
		"https://2130706433/a.jpg":                             "ip",
		"https://0x7f000001/a.jpg":                             "ip",
		"https://[::ffff:127.0.0.1]/a.jpg":                     "ip",
		"ftp://allowed.example.com/a.jpg":                      "https",
		"file:///etc/passwd":                                   "https",
		"https:///a.jpg":                                       "host",
		"https://allowed.example.com/a\x00.jpg":                "invalid",
		"https://allowed.example.com/%2e%2e/etc":               "invalid",
	}
	for u, want := range bad {
		_, err := f.Check(u)
		if err == nil {
			t.Errorf("Check(%q) passed, want a refusal", u)
			continue
		}
		if !errors.Is(err, ferr.ErrBadInput) {
			t.Errorf("Check(%q): kind %v, want bad input", u, ferr.Kind(err))
		}
		if !strings.Contains(strings.ToLower(err.Error()), want) {
			t.Errorf("Check(%q): %q does not mention %q", u, err, want)
		}
	}
}

func TestFetchDisabledWithoutAllowlist(t *testing.T) {
	f, _ := New(Options{})
	if _, err := f.Check("https://example.com/a.jpg"); err == nil || !strings.Contains(err.Error(), "FLATPIX_FETCH_ALLOW") {
		t.Fatalf("err = %v, want a hint about FLATPIX_FETCH_ALLOW", err)
	}
}

func TestBlockedAddresses(t *testing.T) {
	blocked := []string{
		"127.0.0.1:443", "127.255.0.1:443", "10.1.2.3:443", "172.16.0.1:443", "172.31.255.255:443",
		"192.168.1.1:443", "169.254.169.254:80", "0.0.0.0:443", "100.64.0.1:443", "224.0.0.1:443",
		"255.255.255.255:443", "192.0.0.1:443", "198.18.0.1:443", "240.0.0.1:443",
		"[::1]:443", "[::]:443", "[fe80::1]:443", "[fc00::1]:443", "[fd12:3456::1]:443",
		"[::ffff:127.0.0.1]:443", "[::ffff:10.0.0.1]:443", "[ff02::1]:443", "[64:ff9b::7f00:1]:443",
	}
	for _, a := range blocked {
		if err := checkAddress(a); err == nil {
			t.Errorf("checkAddress(%q) allowed a private address", a)
		}
	}
	for _, a := range []string{"93.184.216.34:443", "[2606:4700:4700::1111]:443", "8.8.8.8:443", "172.32.0.1:443", "100.128.0.1:443"} {
		if err := checkAddress(a); err != nil {
			t.Errorf("checkAddress(%q): %v", a, err)
		}
	}
}

// newTestFetcher returns a fetcher that trusts the test server and may reach
// loopback, which production configuration never allows.
func newTestFetcher(t *testing.T, srv *httptest.Server) (*Fetcher, string) {
	t.Helper()
	u, _ := url.Parse(srv.URL)
	f, err := New(Options{Allow: []string{"localhost:" + u.Port()}})
	if err != nil {
		t.Fatal(err)
	}
	f.allowPrivate = true
	f.client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	return f, "https://localhost:" + u.Port()
}

func TestDownload(t *testing.T) {
	body := bytes.Repeat([]byte("abc"), 1000)
	var redirected atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/ok.jpg", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "identity" {
			t.Errorf("Accept-Encoding = %q", r.Header.Get("Accept-Encoding"))
		}
		w.Write(body)
	})
	mux.HandleFunc("/empty", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	mux.HandleFunc("/boom", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 500) })
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/followed", http.StatusFound)
	})
	mux.HandleFunc("/followed", func(w http.ResponseWriter, r *http.Request) { redirected.Add(1); w.Write(body) })
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		// No Content-Length: chunked, so only the byte cap can catch it.
		fl := w.(http.Flusher)
		for i := 0; i < 50; i++ {
			w.Write(bytes.Repeat([]byte("x"), 1000))
			fl.Flush()
		}
	})
	mux.HandleFunc("/declared", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		w.WriteHeader(200)
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("a"))
		w.(http.Flusher).Flush()
		time.Sleep(2 * time.Second)
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	f, base := newTestFetcher(t, srv)

	t.Run("ok", func(t *testing.T) {
		var buf bytes.Buffer
		n, err := f.Download(context.Background(), base+"/ok.jpg", &buf, 1<<20)
		if err != nil || n != int64(len(body)) || !bytes.Equal(buf.Bytes(), body) {
			t.Fatalf("n=%d err=%v", n, err)
		}
	})
	t.Run("empty body is bad input", func(t *testing.T) {
		_, err := f.Download(context.Background(), base+"/empty", &bytes.Buffer{}, 1<<20)
		if !errors.Is(err, ferr.ErrBadInput) {
			t.Fatalf("err = %v, kind %v", err, ferr.Kind(err))
		}
	})
	t.Run("404", func(t *testing.T) {
		_, err := f.Download(context.Background(), base+"/missing", &bytes.Buffer{}, 1<<20)
		if !errors.Is(err, ferr.ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("500 is upstream", func(t *testing.T) {
		_, err := f.Download(context.Background(), base+"/boom", &bytes.Buffer{}, 1<<20)
		if !errors.Is(err, ferr.ErrUpstream) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("redirects are not followed", func(t *testing.T) {
		_, err := f.Download(context.Background(), base+"/redirect", &bytes.Buffer{}, 1<<20)
		if !errors.Is(err, ferr.ErrUpstream) || !strings.Contains(err.Error(), "redirect") {
			t.Fatalf("err = %v", err)
		}
		if redirected.Load() != 0 {
			t.Fatal("the redirect target was requested")
		}
	})
	t.Run("oversize is found by reading max+1", func(t *testing.T) {
		var buf bytes.Buffer
		_, err := f.Download(context.Background(), base+"/big", &buf, 10_000)
		if !errors.Is(err, ferr.ErrTooLarge) {
			t.Fatalf("err = %v", err)
		}
		if buf.Len() > 10_001 {
			t.Fatalf("wrote %d bytes, the cap is max+1 = 10001", buf.Len())
		}
	})
	t.Run("exactly max is fine", func(t *testing.T) {
		var buf bytes.Buffer
		if _, err := f.Download(context.Background(), base+"/ok.jpg", &buf, int64(len(body))); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("declared length over max is refused early", func(t *testing.T) {
		_, err := f.Download(context.Background(), base+"/declared", &bytes.Buffer{}, 1000)
		if !errors.Is(err, ferr.ErrTooLarge) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("context ends the transfer", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := f.Download(ctx, base+"/slow", &bytes.Buffer{}, 1<<20)
		if err == nil || time.Since(start) > time.Second {
			t.Fatalf("err = %v after %v", err, time.Since(start))
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the deadline to show through", err)
		}
	})
}

func TestPrivateAddressIsRefusedAtConnectTime(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("secret")) }))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	// The host is allowlisted by name, but resolves to loopback.
	f, _ := New(Options{Allow: []string{"localhost:" + u.Port()}})
	f.client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	var buf bytes.Buffer
	_, err := f.Download(context.Background(), "https://localhost:"+u.Port()+"/x", &buf, 1<<20)
	if err == nil || buf.Len() != 0 {
		t.Fatalf("a request to loopback went through: err=%v body=%q", err, buf.String())
	}
	if !errors.Is(err, ferr.ErrBadInput) {
		t.Fatalf("kind = %v, want bad input (a policy refusal): %v", ferr.Kind(err), err)
	}
}

func TestLogURL(t *testing.T) {
	tests := map[string]string{
		"https://user:secret@example.com/a/b.jpg?token=abc#frag": "https://example.com/a/b.jpg",
		"https://example.com/a.jpg":                              "https://example.com/a.jpg",
		"https://example.com/a.jpg?X-Amz-Signature=deadbeef":     "https://example.com/a.jpg",
		"::not a url::": "<invalid url>",
	}
	for in, want := range tests {
		if got := LogURL(in); got != want {
			t.Errorf("LogURL(%q) = %q, want %q", in, got, want)
		}
	}
	long := "https://example.com/" + strings.Repeat("a", 5000)
	if got := LogURL(long); len(got) > 1024 {
		t.Errorf("long URL logged with %d bytes, the cap is 1024", len(got))
	}
	if strings.Contains(LogURL("https://u:p@example.com/x?y=1"), "p@") {
		t.Error("credentials leaked into the log URL")
	}
}
