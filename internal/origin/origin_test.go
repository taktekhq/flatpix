// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Taktek and Nizar Mahmoud

package origin

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/taktekhq/flatpix/internal/ferr"
	"github.com/taktekhq/flatpix/internal/s3"
	"github.com/taktekhq/flatpix/internal/s3/s3test"
)

func TestLocalStatAndPath(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "products"), 0o755)
	os.WriteFile(filepath.Join(dir, "products", "red.png"), []byte("pngdata"), 0o644)
	o, err := NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := o.Stat(context.Background(), "products/red.png")
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != 7 || info.Key != "products/red.png" || info.ETag == "" {
		t.Fatalf("info = %+v", info)
	}
	p, ok := o.LocalPath("products/red.png")
	if !ok || p != filepath.Join(dir, "products", "red.png") {
		t.Fatalf("path = %q ok=%v", p, ok)
	}
	// The identity changes when the file changes.
	os.WriteFile(filepath.Join(dir, "products", "red.png"), []byte("other-data"), 0o644)
	info2, _ := o.Stat(context.Background(), "products/red.png")
	if info2.ETag == info.ETag {
		t.Fatal("etag did not change with the file")
	}
}

func TestLocalRefusesEscapes(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "images")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(base, "secret.txt"), []byte("secret"), 0o644)
	os.Symlink(filepath.Join(base, "secret.txt"), filepath.Join(dir, "link.jpg"))
	os.Symlink(base, filepath.Join(dir, "up"))
	os.MkdirAll(filepath.Join(dir, "adir"), 0o755)
	o, _ := NewLocal(dir)
	for _, key := range []string{"../secret.txt", "/etc/passwd", "a/../../secret.txt", "link.jpg", "up/secret.txt", "adir", "", ".", "x\x00y"} {
		_, err := o.Stat(context.Background(), key)
		if err == nil {
			t.Errorf("Stat(%q) succeeded", key)
			continue
		}
		if !errors.Is(err, ferr.ErrNotFound) {
			t.Errorf("Stat(%q): kind %v, want not found", key, ferr.Kind(err))
		}
	}
}

func TestResolveCandidates(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"cat.jpg", "dog.png", "bird", "fox.webp", "owl.jpg", "owl.png"} {
		os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644)
	}
	o, _ := NewLocal(dir)
	tests := []struct {
		id, ext  string
		want     string
		notFound bool
	}{
		{"cat", "jpg", "cat.jpg", false},  // the URL names the file
		{"cat", "", "cat.jpg", false},     // f_auto style URL, no extension
		{"cat", "webp", "cat.jpg", false}, // asks for webp; the source is the jpg
		{"dog", "", "dog.png", false},
		{"bird", "", "bird", false}, // a file with no extension
		{"fox", "jpg", "fox.webp", false},
		{"owl", "", "owl.jpg", false},    // jpg is tried before png
		{"owl", "png", "owl.png", false}, // the URL extension wins when that file exists
		{"nothing", "jpg", "", true},
		{"nothing", "", "", true},
	}
	for _, tc := range tests {
		info, err := Resolve(context.Background(), o, tc.id, tc.ext)
		if tc.notFound {
			if !errors.Is(err, ferr.ErrNotFound) {
				t.Errorf("Resolve(%q,%q) = %+v, %v; want not found", tc.id, tc.ext, info, err)
			}
			continue
		}
		if err != nil || info.Key != tc.want {
			t.Errorf("Resolve(%q,%q) = %q, %v; want %q", tc.id, tc.ext, info.Key, err, tc.want)
		}
	}
}

func newS3(t *testing.T, prefix string) (*S3, *s3test.Server) {
	t.Helper()
	srv := s3test.New()
	t.Cleanup(srv.Close)
	o, err := NewS3(s3.Config{Endpoint: srv.URL, AccessKey: "ak", SecretKey: "sk"}, "media", prefix)
	if err != nil {
		t.Fatal(err)
	}
	return o, srv
}

func TestS3StatAndDownload(t *testing.T) {
	o, srv := newS3(t, "assets/")
	payload := bytes.Repeat([]byte("0123456789"), 100)
	srv.Put("media/assets/folder/cat.jpg", payload, "image/jpeg")

	info, err := o.Stat(context.Background(), "folder/cat.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != 1000 || info.ETag == "" || info.Key != "folder/cat.jpg" {
		t.Fatalf("info = %+v", info)
	}
	var buf bytes.Buffer
	n, err := o.Download(context.Background(), "folder/cat.jpg", &buf, 1<<20)
	if err != nil || n != 1000 || !bytes.Equal(buf.Bytes(), payload) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if _, ok := o.LocalPath("folder/cat.jpg"); ok {
		t.Fatal("an S3 object is not a local file")
	}
	if _, err := o.Stat(context.Background(), "folder/missing.jpg"); !errors.Is(err, ferr.ErrNotFound) {
		t.Fatalf("err = %v, want not found", err)
	}
}

func TestS3DownloadCapsAtMaxPlusOne(t *testing.T) {
	o, srv := newS3(t, "")
	srv.Put("media/big.jpg", bytes.Repeat([]byte("x"), 5000), "image/jpeg")
	var buf bytes.Buffer
	_, err := o.Download(context.Background(), "big.jpg", &buf, 1000)
	if !errors.Is(err, ferr.ErrTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if buf.Len() > 1001 {
		t.Fatalf("wrote %d bytes", buf.Len())
	}
}

func TestS3EmptyObjectIsBadInput(t *testing.T) {
	o, srv := newS3(t, "")
	srv.Put("media/empty.jpg", nil, "image/jpeg")
	_, err := o.Download(context.Background(), "empty.jpg", &bytes.Buffer{}, 1000)
	if !errors.Is(err, ferr.ErrBadInput) {
		t.Fatalf("err = %v", err)
	}
}

func TestS3ResolveUsesHeadOnly(t *testing.T) {
	o, srv := newS3(t, "")
	srv.Put("media/cat.png", []byte("x"), "image/png")
	info, err := Resolve(context.Background(), o, "cat", "jpg")
	if err != nil || info.Key != "cat.png" {
		t.Fatalf("info=%+v err=%v", info, err)
	}
	if srv.Gets.Load() != 0 {
		t.Fatalf("resolving downloaded %d objects", srv.Gets.Load())
	}
}

func TestS3RefusesTraversalKeys(t *testing.T) {
	o, _ := newS3(t, "pub/")
	for _, k := range []string{"../private/x.jpg", "a/../../b.jpg", "/abs.jpg"} {
		if _, err := o.Stat(context.Background(), k); !errors.Is(err, ferr.ErrNotFound) {
			t.Errorf("Stat(%q) = %v", k, err)
		}
	}
}
