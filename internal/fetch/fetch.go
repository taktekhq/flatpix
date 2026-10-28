// Package fetch downloads remote originals for /image/fetch/ URLs, safely.
//
// Rules: https only, hosts (or host/path-prefix) from an allowlist, no IP
// literals, no private, loopback or link-local addresses checked at connect
// time (so DNS tricks fail), no redirects, no credentials in the URL, and a
// byte cap that is read as max+1 to detect oversize.
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/taktekhq/flatpix/internal/ferr"
)

// Options configure a Fetcher.
type Options struct {
	// Allow lists what may be fetched: "host", "*.suffix", "host:port", or
	// "host/path-prefix" (for one bucket on a shared storage host).
	Allow []string
	// UserAgent is sent with every request.
	UserAgent string
	// HeaderTimeout is how long the server may take to send headers.
	HeaderTimeout time.Duration
}

type rule struct {
	host     string // lower case, no port
	wildcard bool   // *.host
	port     string // "443" unless the entry says otherwise
	prefix   string // "" or a path prefix such as /my-bucket
}

// Fetcher downloads allowlisted https URLs.
type Fetcher struct {
	rules  []rule
	client *http.Client
	ua     string
	// allowPrivate lets tests reach loopback servers. It is not configurable
	// from outside the package.
	allowPrivate bool
}

// New builds a Fetcher. With an empty allowlist every fetch is refused.
func New(o Options) (*Fetcher, error) {
	f := &Fetcher{ua: o.UserAgent}
	if f.ua == "" {
		f.ua = "Flatpix"
	}
	for _, e := range o.Allow {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		r := rule{port: "443"}
		if i := strings.IndexByte(e, '/'); i >= 0 {
			r.prefix = path.Clean(e[i:])
			e = e[:i]
		}
		if h, p, err := net.SplitHostPort(e); err == nil {
			e, r.port = h, p
		}
		if strings.HasPrefix(e, "*.") {
			r.wildcard = true
			e = e[2:]
		}
		e = strings.TrimSuffix(e, ".")
		if e == "" || strings.ContainsAny(e, " *@") {
			return nil, fmt.Errorf("fetch: bad allowlist entry %q", e)
		}
		r.host = e
		f.rules = append(f.rules, r)
	}
	hdr := o.HeaderTimeout
	if hdr == 0 {
		hdr = 15 * time.Second
	}
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			if f.allowPrivate {
				return nil
			}
			return checkAddress(address)
		},
	}
	f.client = &http.Client{
		Transport: &http.Transport{
			Proxy:                 nil, // never through a proxy that could bypass the address check
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: hdr,
			DisableCompression:    true,
			MaxIdleConnsPerHost:   4,
			IdleConnTimeout:       30 * time.Second,
			ForceAttemptHTTP2:     true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return f, nil
}

var errBlocked = errors.New("address is not allowed")

// checkAddress refuses every address that is not a normal public unicast one.
func checkAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: %v", errBlocked, err)
	}
	a, err := netip.ParseAddr(host)
	if err != nil || blockedIP(a) {
		return fmt.Errorf("%w: %s", errBlocked, address)
	}
	return nil
}

func blockedIP(a netip.Addr) bool {
	a = a.Unmap()
	if a.Is6() && nat64.Contains(a) {
		// NAT64 maps IPv4 into 64:ff9b::/96. Check the embedded address too.
		b := a.As16()
		if blockedIP(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})) {
			return true
		}
	}
	return a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() || !a.IsGlobalUnicast() ||
		(a.Is4() && blockedV4(a))
}

var (
	nat64      = netip.MustParsePrefix("64:ff9b::/96")
	blockedV4s = []netip.Prefix{
		netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
		netip.MustParsePrefix("192.0.0.0/24"),  // IETF protocol assignments
		netip.MustParsePrefix("192.0.2.0/24"),  // documentation
		netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
		netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"),
		netip.MustParsePrefix("240.0.0.0/4"), // reserved, includes broadcast
	}
)

func blockedV4(a netip.Addr) bool {
	for _, p := range blockedV4s {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func bad(format string, a ...any) error { return ferr.New(ferr.ErrBadInput, format, a...) }

// Check validates a URL against the policy and returns it parsed.
func (f *Fetcher) Check(raw string) (*url.URL, error) {
	if len(f.rules) == 0 {
		return nil, bad("fetching remote images is disabled: set FLATPIX_FETCH_ALLOW to the hosts you trust")
	}
	if strings.ContainsAny(raw, "\x00\r\n") {
		return nil, bad("invalid fetch url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, bad("invalid fetch url")
	}
	if u.Scheme != "https" {
		return nil, bad("only https urls can be fetched")
	}
	if u.User != nil {
		return nil, bad("credentials in the fetch url are not allowed")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return nil, bad("fetch url has no host")
	}
	if looksLikeIP(host) {
		return nil, bad("fetching from an ip address is not allowed")
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	// Reject traversal and odd encodings in the path before matching a prefix.
	p := u.EscapedPath()
	for _, seg := range strings.Split(p, "/") {
		low := strings.ToLower(seg)
		if seg == ".." || low == "%2e%2e" || low == ".%2e" || low == "%2e." || strings.Contains(low, "%2f") || strings.Contains(low, "%5c") {
			return nil, bad("invalid path in fetch url")
		}
	}
	upath := u.Path
	for _, r := range f.rules {
		if r.port != port || !r.matchHost(host) {
			continue
		}
		if r.prefix != "" && r.prefix != "/" {
			if upath != r.prefix && !strings.HasPrefix(upath, r.prefix+"/") {
				continue
			}
		}
		return u, nil
	}
	return nil, bad("host %q is not on the fetch allowlist", host)
}

func (r rule) matchHost(host string) bool {
	if r.wildcard {
		return strings.HasSuffix(host, "."+r.host)
	}
	return host == r.host
}

// looksLikeIP catches IP literals in every spelling: dotted, decimal, hex,
// octal and IPv6.
func looksLikeIP(host string) bool {
	if strings.Contains(host, ":") {
		return true
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	// The last label of a real host name is never all digits or 0x...
	last := host
	if i := strings.LastIndexByte(host, '.'); i >= 0 {
		last = host[i+1:]
	}
	if last == "" {
		return false
	}
	if strings.HasPrefix(last, "0x") {
		return true
	}
	for _, c := range last {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// LogURL is safe to log: no credentials, query or fragment, at most 1 KB.
func LogURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "<invalid url>"
	}
	s := u.Scheme + "://" + u.Host + u.EscapedPath()
	if len(s) > 1024 {
		s = s[:1024]
	}
	return s
}

// Download fetches raw into dst, reading at most max bytes. A body of more
// than max bytes is ErrTooLarge (it reads max+1 to know). An empty body is
// bad input. It returns the number of bytes written.
func (f *Fetcher) Download(ctx context.Context, raw string, dst io.Writer, max int64) (int64, error) {
	u, err := f.Check(raw)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, bad("invalid fetch url")
	}
	req.Header.Set("User-Agent", f.ua)
	req.Header.Set("Accept", "image/*,*/*;q=0.5")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := f.client.Do(req)
	if err != nil {
		return 0, f.wrapErr(ctx, u, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return 0, ferr.New(ferr.ErrUpstream, "the remote server answered with a redirect (%d); redirects are not followed", resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return 0, ferr.New(ferr.ErrNotFound, "the remote image was not found")
	default:
		return 0, ferr.New(ferr.ErrUpstream, "the remote server answered %d", resp.StatusCode)
	}
	if resp.ContentLength > max {
		return 0, ferr.New(ferr.ErrTooLarge, "the remote image is %d bytes, the limit is %d", resp.ContentLength, max)
	}
	n, err := io.Copy(dst, io.LimitReader(resp.Body, max+1))
	if err != nil {
		return n, f.wrapErr(ctx, u, err)
	}
	if n > max {
		return n, ferr.New(ferr.ErrTooLarge, "the remote image is over the limit of %d bytes", max)
	}
	if n == 0 {
		return 0, bad("the remote image is empty")
	}
	return n, nil
}

func (f *Fetcher) wrapErr(ctx context.Context, u *url.URL, err error) error {
	if ctx.Err() != nil {
		return ctx.Err() // the caller's budget or the client ended it
	}
	if errors.Is(err, errBlocked) {
		return bad("host %q resolves to an address that is not allowed", u.Hostname())
	}
	return ferr.Wrap(ferr.ErrUpstream, err, "could not fetch the remote image")
}
