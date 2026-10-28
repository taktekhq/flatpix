package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/taktekhq/flatpix/internal/sniff"
)

// Disk stores outputs as files: <dir>/<first two hex>/<key>.
type Disk struct{ dir string }

// NewDisk opens (and creates) a cache directory.
func NewDisk(dir string) (*Disk, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	return &Disk{dir: dir}, nil
}

func (d *Disk) path(key string) string { return filepath.Join(d.dir, key[:2], key) }

func (d *Disk) Get(_ context.Context, key string) (*Entry, error) {
	f, err := os.Open(d.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		f.Close()
		return nil, nil // an empty file is never a valid output
	}
	head := make([]byte, sniff.HeadSize)
	n, _ := f.ReadAt(head, 0)
	return &Entry{Body: f, Size: st.Size(), ContentType: sniff.Sniff(head[:n]).ContentType()}, nil
}

func (d *Disk) Put(_ context.Context, key, srcPath string) error {
	dst := d.path(key)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".put-*")
	if err != nil {
		return err
	}
	_, err = io.Copy(tmp, src)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}
