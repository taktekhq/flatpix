// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Taktek and Nizar Mahmoud

package cache

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/taktekhq/flatpix/internal/s3"
	"github.com/taktekhq/flatpix/internal/s3/s3test"
)

func base() KeyParts {
	return KeyParts{Cloud: "demo", Source: "cat.jpg", ResolvedKey: "cat.jpg", Identity: "etag:abc",
		Transform: "c_fill,h_300,w_300", Format: "webp"}
}

func TestKeyChangesWithEveryField(t *testing.T) {
	k0 := Key(base())
	if k0 != Key(base()) {
		t.Fatal("key is not deterministic")
	}
	if len(k0) != 64 {
		t.Fatalf("key length = %d", len(k0))
	}
	mut := map[string]func(*KeyParts){
		"cloud":     func(p *KeyParts) { p.Cloud = "other" },
		"source":    func(p *KeyParts) { p.Source = "cat.png" },
		"resolved":  func(p *KeyParts) { p.ResolvedKey = "cat.png" },
		"identity":  func(p *KeyParts) { p.Identity = "etag:abd" },
		"transform": func(p *KeyParts) { p.Transform = "c_fill,h_300,w_301" },
		"format":    func(p *KeyParts) { p.Format = "avif" },
	}
	for name, f := range mut {
		p := base()
		f(&p)
		if Key(p) == k0 {
			t.Errorf("changing %s did not change the key", name)
		}
	}
}

func TestSourcesThatDifferOnlyByExtensionNeverShareAKey(t *testing.T) {
	a := KeyParts{Cloud: "demo", Source: "cat", ResolvedKey: "cat.jpg", Identity: "etag:1", Transform: "w_100", Format: "webp"}
	b := a
	b.ResolvedKey = "cat.png"
	if Key(a) == Key(b) {
		t.Fatal("cat.jpg and cat.png share a key (same etag, same everything else)")
	}
	// A versioned URL has no resolved key; the extension in the URL still counts.
	c := KeyParts{Cloud: "demo", Source: "cat.jpg", Identity: "v:12", Transform: "w_100", Format: "webp"}
	d := c
	d.Source = "cat.png"
	if Key(c) == Key(d) {
		t.Fatal("versioned cat.jpg and cat.png share a key")
	}
}

func TestFieldsCannotBleedIntoEachOther(t *testing.T) {
	a := KeyParts{Cloud: "ab", Source: "c"}
	b := KeyParts{Cloud: "a", Source: "bc"}
	if Key(a) == Key(b) {
		t.Fatal("field boundaries are ambiguous")
	}
}

var jpegHead = []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F', 0, 1, 1, 0, 0, 1, 0, 1, 0, 0}

func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "out")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDiskRoundTrip(t *testing.T) {
	d, err := NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := Key(base())
	if e, err := d.Get(context.Background(), key); e != nil || err != nil {
		t.Fatalf("empty cache returned %v, %v", e, err)
	}
	data := append(append([]byte{}, jpegHead...), bytes.Repeat([]byte("z"), 500)...)
	if err := d.Put(context.Background(), key, writeTemp(t, data)); err != nil {
		t.Fatal(err)
	}
	e, err := d.Get(context.Background(), key)
	if err != nil || e == nil {
		t.Fatalf("get after put: %v, %v", e, err)
	}
	defer e.Body.Close()
	got, _ := io.ReadAll(e.Body)
	if !bytes.Equal(got, data) || e.Size != int64(len(data)) || e.ContentType != "image/jpeg" {
		t.Fatalf("size %d type %q", e.Size, e.ContentType)
	}
	if _, ok := e.Body.(io.ReadSeeker); !ok {
		t.Fatal("a disk hit should be seekable so ranges work")
	}
}

func TestDiskIgnoresEmptyAndTornFiles(t *testing.T) {
	dir := t.TempDir()
	d, _ := NewDisk(dir)
	key := Key(base())
	os.MkdirAll(filepath.Join(dir, key[:2]), 0o755)
	os.WriteFile(filepath.Join(dir, key[:2], key), nil, 0o644)
	if e, err := d.Get(context.Background(), key); e != nil || err != nil {
		t.Fatalf("an empty file must be a miss, got %v %v", e, err)
	}
}

func TestDiskPutLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	d, _ := NewDisk(dir)
	key := Key(base())
	if err := d.Put(context.Background(), key, writeTemp(t, jpegHead)); err != nil {
		t.Fatal(err)
	}
	var files []string
	filepath.WalkDir(dir, func(p string, de os.DirEntry, err error) error {
		if err == nil && !de.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if len(files) != 1 {
		t.Fatalf("files = %v", files)
	}
}

func TestS3RoundTrip(t *testing.T) {
	srv := s3test.New()
	defer srv.Close()
	st, err := NewS3(s3.Config{Endpoint: srv.URL, AccessKey: "a", SecretKey: "b"}, "derived", "v1/")
	if err != nil {
		t.Fatal(err)
	}
	key := Key(base())
	if e, err := st.Get(context.Background(), key); e != nil || err != nil {
		t.Fatalf("empty cache returned %v, %v", e, err)
	}
	data := append(append([]byte{}, jpegHead...), bytes.Repeat([]byte("q"), 300)...)
	if err := st.Put(context.Background(), key, writeTemp(t, data)); err != nil {
		t.Fatal(err)
	}
	if _, ok := srv.Get("derived/v1/" + key[:2] + "/" + key); !ok {
		t.Fatalf("object not stored where expected; have %d objects", srv.Len())
	}
	e, err := st.Get(context.Background(), key)
	if err != nil || e == nil {
		t.Fatalf("get: %v, %v", e, err)
	}
	defer e.Body.Close()
	got, _ := io.ReadAll(e.Body)
	if !bytes.Equal(got, data) || e.Size != int64(len(data)) || e.ContentType != "image/jpeg" {
		t.Fatalf("size %d type %q", e.Size, e.ContentType)
	}
}
