// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Taktek and Nizar Mahmoud

package origin

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/taktekhq/flatpix/internal/ferr"
)

// Local serves originals from a directory. It uses os.Root, so neither ".."
// nor a symlink can reach outside the directory.
type Local struct {
	dir  string
	root *os.Root
}

// NewLocal opens dir as an origin.
func NewLocal(dir string) (*Local, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("origin: %w", err)
	}
	return &Local{dir: abs, root: r}, nil
}

func (l *Local) Stat(_ context.Context, key string) (Info, error) {
	k, ok := cleanKey(key)
	if !ok {
		return Info{}, ferr.New(ferr.ErrNotFound, "image not found")
	}
	st, err := l.root.Stat(filepath.FromSlash(k))
	if err != nil || !st.Mode().IsRegular() {
		return Info{}, ferr.New(ferr.ErrNotFound, "image not found")
	}
	// Reject a symlink that points outside: Stat on a root follows links
	// inside the root only, so a link out fails above.
	return Info{Key: k, Size: st.Size(), ETag: fmt.Sprintf("%x-%x", st.Size(), st.ModTime().UnixNano())}, nil
}

func (l *Local) LocalPath(key string) (string, bool) {
	k, ok := cleanKey(key)
	if !ok {
		return "", false
	}
	return filepath.Join(l.dir, filepath.FromSlash(k)), true
}

func (l *Local) Download(_ context.Context, key string, w io.Writer, max int64) (int64, error) {
	k, ok := cleanKey(key)
	if !ok {
		return 0, ferr.New(ferr.ErrNotFound, "image not found")
	}
	f, err := l.root.Open(filepath.FromSlash(k))
	if err != nil {
		return 0, ferr.New(ferr.ErrNotFound, "image not found")
	}
	defer f.Close()
	return copyCapped(w, f, max)
}

// copyCapped copies at most max bytes, reading max+1 to notice oversize.
func copyCapped(w io.Writer, r io.Reader, max int64) (int64, error) {
	n, err := io.Copy(w, io.LimitReader(r, max+1))
	if err != nil {
		return n, err
	}
	if n > max {
		return n, ferr.New(ferr.ErrTooLarge, "the original is over the limit of %d bytes", max)
	}
	if n == 0 {
		return 0, ferr.New(ferr.ErrBadInput, "the original is empty")
	}
	return n, nil
}
