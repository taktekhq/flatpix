// Package scratch owns the one directory where Flatpix puts every temporary
// file: downloaded originals, rendered outputs and the spool files of child
// tools such as Ghostscript and ImageMagick.
package scratch

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/taktekhq/flatpix/internal/gate"
)

// Dir is the scratch directory plus a byte budget for what lives in it.
type Dir struct {
	path   string
	budget *gate.Gate
}

// Open creates the directory if needed and empties it. It removes the
// contents and keeps the directory itself, because the directory may be a
// mount point. budgetBytes caps what downloads may hold at one time.
func Open(path string, budgetBytes int64) (*Dir, error) {
	if path == "" {
		return nil, fmt.Errorf("scratch: empty path")
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nil, fmt.Errorf("scratch: %w", err)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("scratch: %w", err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(path, e.Name())); err != nil {
			return nil, fmt.Errorf("scratch: emptying: %w", err)
		}
	}
	return &Dir{path: path, budget: gate.New(budgetBytes)}, nil
}

// Path is the directory.
func (d *Dir) Path() string { return d.path }

// Budget is the total byte budget.
func (d *Dir) Budget() int64 { return d.budget.Capacity() }

// BudgetUsed is how many bytes are reserved or charged now.
func (d *Dir) BudgetUsed() int64 { return d.budget.InUse() }

// BudgetWaiting is how many reservations are queued.
func (d *Dir) BudgetWaiting() int { return d.budget.Waiting() }

// NewFile creates a new file in the scratch directory.
func (d *Dir) NewFile(pattern string) (*os.File, error) {
	return os.CreateTemp(d.path, pattern)
}

// Reserve waits (first come, first served) for n bytes of the budget. A
// download calls it before writing, so a burst of large distinct originals
// cannot fill the disk. It returns a lease: shrink it to the real size when known, release it when the file is deleted.
func (d *Dir) Reserve(ctx context.Context, n int64) (*gate.Lease, error) {
	if n > d.budget.Capacity() {
		n = d.budget.Capacity()
	}
	return d.budget.AcquireLease(ctx, n)
}

// Charge counts n bytes that already exist, such as a rendered output, without
// waiting. It returns a release function.
func (d *Dir) Charge(n int64) (release func()) { return d.budget.Force(n) }

// Env is the environment that points a child tool's temporary files at the
// scratch directory.
func (d *Dir) Env() []string {
	return []string{"TMPDIR=" + d.path, "MAGICK_TMPDIR=" + d.path}
}

// Sweep removes entries whose modification time is older than maxAge. It
// returns how many it removed.
func (d *Dir) Sweep(maxAge time.Duration, now time.Time) int {
	entries, err := os.ReadDir(d.path)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) <= maxAge {
			continue
		}
		if err := os.RemoveAll(filepath.Join(d.path, e.Name())); err == nil {
			n++
		}
	}
	return n
}

// RunSweeper calls Sweep every interval until ctx ends. Flatpix uses
// 2 minutes and 10 minutes.
func (d *Dir) RunSweeper(ctx context.Context, interval, maxAge time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if n := d.Sweep(maxAge, now); n > 0 {
				slog.Info("scratch_swept", "removed", n)
			}
		}
	}
}
