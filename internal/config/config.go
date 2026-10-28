// Package config reads Flatpix settings from environment variables and flags.
// A flag overrides its environment variable.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/taktekhq/flatpix/internal/cache"
	"github.com/taktekhq/flatpix/internal/origin"
	"github.com/taktekhq/flatpix/internal/s3"
)

// HelpError is returned by Load for -h and -help.
type HelpError struct{ Usage string }

func (e *HelpError) Error() string {
	return "flatpix: a self-hosted image server for Cloudinary-style URLs\n" + e.Usage
}

// Config is everything the server is told.
type Config struct {
	Listen string

	Seats     int // concurrent renders
	Downloads int // concurrent downloads

	QueueTimeout    time.Duration
	DownloadTimeout time.Duration
	RenderTimeout   time.Duration
	ShutdownTimeout time.Duration

	MaxFileMB      int
	MaxPixels      int64 // output
	MaxInputPixels int64

	Origin string            // default origin: a path, dir:/path or s3://bucket/prefix
	Clouds map[string]string // per cloud name overrides
	Cache  string            // "" means no derived cache
	S3     s3.Config

	FetchAllow []string
	Lenient    bool

	ScratchDir string
	ScratchMB  int64

	VipsConcurrency int
	Isolate         bool
	LogJSON         bool

	ShowVersion bool
}

// MaxFileBytes is the original file size limit in bytes.
func (c *Config) MaxFileBytes() int64 { return int64(c.MaxFileMB) << 20 }

type getenv func(string) string

func (g getenv) str(key, def string) string {
	if v := g(key); v != "" {
		return v
	}
	return def
}

// Load parses flags (args, without the program name) over the environment.
func Load(args []string, env func(string) string) (*Config, error) {
	g := getenv(env)
	var errs []string
	fail := func(key, why string) { errs = append(errs, key+": "+why) }

	intv := func(key string, def int) int {
		v := g(key)
		if v == "" {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			fail(key, fmt.Sprintf("want a whole number above 0, got %q", v))
			return def
		}
		return n
	}
	int64v := func(key string, def int64) int64 {
		v := g(key)
		if v == "" {
			return def
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			fail(key, fmt.Sprintf("want a whole number above 0, got %q", v))
			return def
		}
		return n
	}
	dur := func(key string, def time.Duration) time.Duration {
		v := g(key)
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			fail(key, fmt.Sprintf("want a duration such as 10s, got %q", v))
			return def
		}
		return d
	}
	boolv := func(key string) bool {
		switch strings.ToLower(g(key)) {
		case "", "0", "false", "no", "off":
			return false
		case "1", "true", "yes", "on":
			return true
		}
		fail(key, fmt.Sprintf("want 1 or 0, got %q", g(key)))
		return false
	}

	c := &Config{
		Listen:          g.str("FLATPIX_LISTEN", ":8080"),
		Seats:           intv("FLATPIX_SEATS", runtime.NumCPU()),
		QueueTimeout:    dur("FLATPIX_QUEUE_TIMEOUT", 10*time.Second),
		DownloadTimeout: dur("FLATPIX_DOWNLOAD_TIMEOUT", 30*time.Second),
		RenderTimeout:   dur("FLATPIX_RENDER_TIMEOUT", 60*time.Second),
		ShutdownTimeout: dur("FLATPIX_SHUTDOWN_TIMEOUT", 30*time.Second),
		MaxFileMB:       intv("FLATPIX_MAX_FILE_MB", 40),
		MaxPixels:       int64v("FLATPIX_MAX_PIXELS", 25_000_000),
		MaxInputPixels:  int64v("FLATPIX_MAX_INPUT_PIXELS", 100_000_000),
		Origin:          g.str("FLATPIX_ORIGIN", "./images"),
		Cache:           g("FLATPIX_CACHE"),
		Lenient:         boolv("FLATPIX_LENIENT"),
		ScratchDir:      g.str("FLATPIX_TMPDIR", os.TempDir()+"/flatpix"),
		ScratchMB:       int64v("FLATPIX_SCRATCH_MB", 2048),
		Isolate:         boolv("FLATPIX_ISOLATE"),
		LogJSON:         strings.EqualFold(g("FLATPIX_LOG"), "json"),
		S3: s3.Config{
			Endpoint:  g("FLATPIX_S3_ENDPOINT"),
			Region:    g.str("FLATPIX_S3_REGION", g("AWS_REGION")),
			AccessKey: g.str("FLATPIX_S3_ACCESS_KEY", g("AWS_ACCESS_KEY_ID")),
			SecretKey: g.str("FLATPIX_S3_SECRET_KEY", g("AWS_SECRET_ACCESS_KEY")),
		},
	}
	c.Downloads = intv("FLATPIX_DOWNLOADS", max(2, c.Seats*2))
	c.VipsConcurrency = intv("FLATPIX_VIPS_CONCURRENCY", max(1, runtime.NumCPU()/c.Seats))
	for _, h := range strings.Split(g("FLATPIX_FETCH_ALLOW"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			c.FetchAllow = append(c.FetchAllow, h)
		}
	}
	if v := g("FLATPIX_CLOUDS"); v != "" {
		c.Clouds = map[string]string{}
		for _, kv := range strings.Split(v, ",") {
			name, spec, ok := strings.Cut(strings.TrimSpace(kv), "=")
			if !ok || name == "" || spec == "" {
				fail("FLATPIX_CLOUDS", fmt.Sprintf("want name=origin pairs, got %q", kv))
				continue
			}
			c.Clouds[strings.TrimSpace(name)] = strings.TrimSpace(spec)
		}
	}

	fs := flag.NewFlagSet("flatpix", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&c.Listen, "listen", c.Listen, "address to listen on (FLATPIX_LISTEN)")
	fs.IntVar(&c.Seats, "seats", c.Seats, "concurrent renders (FLATPIX_SEATS)")
	fs.DurationVar(&c.QueueTimeout, "queue-timeout", c.QueueTimeout, "how long a render waits for a seat (FLATPIX_QUEUE_TIMEOUT)")
	fs.IntVar(&c.MaxFileMB, "max-file-mb", c.MaxFileMB, "largest original in MB (FLATPIX_MAX_FILE_MB)")
	fs.Int64Var(&c.MaxPixels, "max-pixels", c.MaxPixels, "most pixels in an output (FLATPIX_MAX_PIXELS)")
	fs.StringVar(&c.Origin, "origin", c.Origin, "default origin (FLATPIX_ORIGIN)")
	fs.StringVar(&c.Cache, "cache", c.Cache, "derived cache (FLATPIX_CACHE)")
	fs.BoolVar(&c.Lenient, "lenient", c.Lenient, "ignore unknown parameters (FLATPIX_LENIENT)")
	fs.StringVar(&c.ScratchDir, "tmpdir", c.ScratchDir, "scratch directory (FLATPIX_TMPDIR)")
	fs.BoolVar(&c.ShowVersion, "version", false, "print the version and exit")
	usage := &strings.Builder{}
	fs.SetOutput(usage)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, &HelpError{Usage: flagUsage(fs)}
		}
		return nil, fmt.Errorf("%v\n%s", err, flagUsage(fs))
	}
	if c.Seats < 1 {
		fail("seats", "must be at least 1")
	}
	if c.QueueTimeout <= 0 {
		fail("queue-timeout", "must be above 0")
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("bad configuration:\n  %s", strings.Join(errs, "\n  "))
	}
	return c, nil
}

func flagUsage(fs *flag.FlagSet) string {
	var b strings.Builder
	b.WriteString("flags:\n")
	fs.SetOutput(&b)
	fs.PrintDefaults()
	return b.String()
}

// ParseSpec splits an origin or cache spec into its kind ("dir" or "s3") and
// the rest. A bare path is a directory.
func ParseSpec(spec string) (kind, rest string, err error) {
	switch {
	case spec == "":
		return "", "", fmt.Errorf("empty origin")
	case strings.HasPrefix(spec, "s3://"):
		r := strings.TrimPrefix(spec, "s3://")
		if r == "" || strings.HasPrefix(r, "/") {
			return "", "", fmt.Errorf("%q needs a bucket name", spec)
		}
		return "s3", r, nil
	case strings.HasPrefix(spec, "dir:"):
		return "dir", strings.TrimPrefix(spec, "dir:"), nil
	case strings.Contains(spec, "://"):
		return "", "", fmt.Errorf("%q: only directories and s3:// are supported", spec)
	}
	return "dir", spec, nil
}

func splitBucket(rest string) (bucket, prefix string) {
	bucket, prefix, _ = strings.Cut(rest, "/")
	return bucket, prefix
}

// OpenOrigin opens an origin from a spec.
func (c *Config) OpenOrigin(spec string) (origin.Origin, error) {
	kind, rest, err := ParseSpec(spec)
	if err != nil {
		return nil, err
	}
	if kind == "s3" {
		b, p := splitBucket(rest)
		return origin.NewS3(c.S3, b, p)
	}
	return origin.NewLocal(rest)
}

// OpenCache opens the derived cache, or returns nil if none is configured.
func (c *Config) OpenCache() (cache.Store, error) {
	if c.Cache == "" {
		return nil, nil
	}
	kind, rest, err := ParseSpec(c.Cache)
	if err != nil {
		return nil, err
	}
	if kind == "s3" {
		b, p := splitBucket(rest)
		return cache.NewS3(c.S3, b, p)
	}
	return cache.NewDisk(rest)
}

// Summary lists the settings for the startup log, without any secret.
func (c *Config) Summary() []any {
	secret := func(s string) string {
		if s == "" {
			return "unset"
		}
		return "set"
	}
	return []any{
		"listen", c.Listen, "seats", c.Seats, "downloads", c.Downloads, "vips_threads", c.VipsConcurrency,
		"queue_timeout", c.QueueTimeout.String(), "download_timeout", c.DownloadTimeout.String(), "render_timeout", c.RenderTimeout.String(),
		"max_file_mb", c.MaxFileMB, "max_output_pixels", c.MaxPixels, "max_input_pixels", c.MaxInputPixels,
		"origin", c.Origin, "clouds", len(c.Clouds), "cache", c.Cache, "fetch_allow", len(c.FetchAllow),
		"lenient", c.Lenient, "isolate", c.Isolate, "scratch", c.ScratchDir, "scratch_mb", c.ScratchMB,
		"s3_endpoint", c.S3.Endpoint, "s3_access_key", secret(c.S3.AccessKey), "s3_secret_key", secret(c.S3.SecretKey),
	}
}
