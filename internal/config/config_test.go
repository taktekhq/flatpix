package config

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

func env(kv ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return func(k string) string { return m[k] }
}

func TestDefaults(t *testing.T) {
	c, err := Load(nil, env())
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.Seats != runtime.NumCPU() || c.QueueTimeout != 10*time.Second ||
		c.MaxFileMB != 40 || c.MaxFileBytes() != 40<<20 || c.Origin != "./images" || c.Cache != "" ||
		c.Lenient || len(c.FetchAllow) != 0 {
		t.Fatalf("defaults = %+v", c)
	}
	if c.MaxPixels != 25_000_000 || c.MaxInputPixels != 100_000_000 {
		t.Errorf("pixel limits = %d, %d", c.MaxPixels, c.MaxInputPixels)
	}
	if c.VipsConcurrency < 1 {
		t.Errorf("vips concurrency = %d", c.VipsConcurrency)
	}
}

func TestEnvAndFlagsFlagsWin(t *testing.T) {
	c, err := Load([]string{"-seats", "7", "-listen", ":9000"}, env(
		"FLATPIX_SEATS", "3", "FLATPIX_LISTEN", ":8081", "FLATPIX_QUEUE_TIMEOUT", "2s",
		"FLATPIX_MAX_FILE_MB", "5", "FLATPIX_MAX_PIXELS", "1000000", "FLATPIX_ORIGIN", "/data",
		"FLATPIX_CACHE", "s3://derived/v1", "FLATPIX_FETCH_ALLOW", "a.com, *.b.com ,c.com/bucket",
		"FLATPIX_LENIENT", "1", "FLATPIX_S3_ENDPOINT", "http://minio:9000", "FLATPIX_S3_ACCESS_KEY", "k",
		"FLATPIX_S3_SECRET_KEY", "s", "FLATPIX_S3_REGION", "auto",
	))
	if err != nil {
		t.Fatal(err)
	}
	if c.Seats != 7 || c.Listen != ":9000" {
		t.Errorf("flags should win: seats %d listen %q", c.Seats, c.Listen)
	}
	if c.QueueTimeout != 2*time.Second || c.MaxFileMB != 5 || c.MaxPixels != 1_000_000 || c.Origin != "/data" ||
		c.Cache != "s3://derived/v1" || !c.Lenient {
		t.Errorf("env not applied: %+v", c)
	}
	if strings.Join(c.FetchAllow, "|") != "a.com|*.b.com|c.com/bucket" {
		t.Errorf("allow = %q", c.FetchAllow)
	}
	if c.S3.Endpoint != "http://minio:9000" || c.S3.AccessKey != "k" || c.S3.SecretKey != "s" || c.S3.Region != "auto" {
		t.Errorf("s3 = %+v", c.S3)
	}
}

func TestAWSVariablesAreAFallback(t *testing.T) {
	c, _ := Load(nil, env("AWS_ACCESS_KEY_ID", "ak", "AWS_SECRET_ACCESS_KEY", "sk", "AWS_REGION", "eu-west-1"))
	if c.S3.AccessKey != "ak" || c.S3.SecretKey != "sk" || c.S3.Region != "eu-west-1" {
		t.Errorf("s3 = %+v", c.S3)
	}
	c, _ = Load(nil, env("AWS_ACCESS_KEY_ID", "ak", "FLATPIX_S3_ACCESS_KEY", "mine"))
	if c.S3.AccessKey != "mine" {
		t.Errorf("FLATPIX_ variable should win, got %q", c.S3.AccessKey)
	}
}

func TestBadValues(t *testing.T) {
	for name, e := range map[string]func(string) string{
		"seats":    env("FLATPIX_SEATS", "zero"),
		"seats0":   env("FLATPIX_SEATS", "0"),
		"timeout":  env("FLATPIX_QUEUE_TIMEOUT", "soon"),
		"timeout0": env("FLATPIX_QUEUE_TIMEOUT", "0s"),
		"file":     env("FLATPIX_MAX_FILE_MB", "-1"),
		"pixels":   env("FLATPIX_MAX_PIXELS", "big"),
		"lenient":  env("FLATPIX_LENIENT", "maybe"),
		"clouds":   env("FLATPIX_CLOUDS", "nope"),
	} {
		if _, err := Load(nil, e); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, err := Load([]string{"-bogus"}, env()); err == nil {
		t.Error("unknown flag should fail")
	}
}

func TestBooleans(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on"} {
		if c, err := Load(nil, env("FLATPIX_LENIENT", v)); err != nil || !c.Lenient {
			t.Errorf("%q should be true (%v)", v, err)
		}
	}
	for _, v := range []string{"0", "false", "no", "off", ""} {
		if c, err := Load(nil, env("FLATPIX_LENIENT", v)); err != nil || c.Lenient {
			t.Errorf("%q should be false (%v)", v, err)
		}
	}
}

func TestClouds(t *testing.T) {
	c, err := Load(nil, env("FLATPIX_CLOUDS", "demo=dir:/a, acme=s3://bucket/prefix"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Clouds["demo"] != "dir:/a" || c.Clouds["acme"] != "s3://bucket/prefix" || len(c.Clouds) != 2 {
		t.Errorf("clouds = %v", c.Clouds)
	}
}

func TestParseSpec(t *testing.T) {
	tests := []struct {
		in         string
		kind, rest string
		bad        bool
	}{
		{"/srv/images", "dir", "/srv/images", false},
		{"./images", "dir", "./images", false},
		{"dir:/srv/x", "dir", "/srv/x", false},
		{"images", "dir", "images", false},
		{"s3://media", "s3", "media", false},
		{"s3://media/assets/2024", "s3", "media/assets/2024", false},
		{"s3://", "", "", true},
		{"ftp://x", "", "", true},
		{"", "", "", true},
	}
	for _, tc := range tests {
		k, r, err := ParseSpec(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("ParseSpec(%q) should fail", tc.in)
			}
			continue
		}
		if err != nil || k != tc.kind || r != tc.rest {
			t.Errorf("ParseSpec(%q) = %q %q %v", tc.in, k, r, err)
		}
	}
}

func TestSummaryHidesSecrets(t *testing.T) {
	c, _ := Load(nil, env("FLATPIX_S3_ACCESS_KEY", "AKIA-SECRET-ID", "FLATPIX_S3_SECRET_KEY", "super-secret-key"))
	for _, v := range c.Summary() {
		if s, ok := v.(string); ok && (strings.Contains(s, "AKIA-SECRET-ID") || strings.Contains(s, "super-secret-key")) {
			t.Errorf("summary leaks a key: %q", s)
		}
	}
}
