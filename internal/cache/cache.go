// Package cache stores rendered outputs so each one is rendered once and
// served from storage after that. There are two stores: a directory and an
// S3-compatible bucket. Entries are never expired by Flatpix; keys already
// include everything that changes the output.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strconv"
)

// KeyParts is everything that changes the output bytes.
type KeyParts struct {
	// Cloud is the cloud name from the URL.
	Cloud string
	// Source is the source path as requested, with its extension, or the
	// remote URL for fetch.
	Source string
	// ResolvedKey is the stored object that was found, with its extension.
	// It is empty when the URL carries a version and no lookup was needed.
	ResolvedKey string
	// Identity is the version ("v:123") or the etag ("etag:abc") of the source.
	Identity string
	// Transform is transform.Spec.Normalize().
	Transform string
	// Format is the output format after negotiation ("webp", or "orig").
	Format string
}

// keyVersion changes when the renderer changes in a way that alters output.
const keyVersion = "flatpix-key-1"

// Key returns the hex SHA-256 over the length-prefixed fields.
func Key(p KeyParts) string {
	h := sha256.New()
	for _, f := range []string{keyVersion, p.Cloud, p.Source, p.ResolvedKey, p.Identity, p.Transform, p.Format} {
		io.WriteString(h, strconv.Itoa(len(f)))
		io.WriteString(h, ":")
		io.WriteString(h, f)
		io.WriteString(h, ";")
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Entry is a stored output. Body is an io.ReadSeeker when the store can seek.
type Entry struct {
	Body        io.ReadCloser
	Size        int64
	ContentType string
}

// Store keeps rendered outputs.
type Store interface {
	// Get returns the entry, or nil, nil when there is none.
	Get(ctx context.Context, key string) (*Entry, error)
	// Put stores the file at srcPath under key.
	Put(ctx context.Context, key, srcPath string) error
}
