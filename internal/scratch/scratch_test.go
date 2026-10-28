// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Taktek and Nizar Mahmoud

package scratch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenEmptiesContentsButKeepsTheDirectory(t *testing.T) {
	root := t.TempDir() // stands in for a mount point
	os.WriteFile(filepath.Join(root, "old.tmp"), []byte("x"), 0o600)
	os.MkdirAll(filepath.Join(root, "sub", "deeper"), 0o700)
	os.WriteFile(filepath.Join(root, "sub", "deeper", "f"), []byte("y"), 0o600)
	before, _ := os.Stat(root)

	d, err := Open(root, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatalf("scratch not empty after Open: %v", entries)
	}
	after, _ := os.Stat(root)
	if !os.SameFile(before, after) {
		t.Fatal("Open replaced the directory itself")
	}
	if d.Path() != root {
		t.Fatalf("path = %q", d.Path())
	}
}

func TestOpenCreatesMissingDirectory(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a", "b")
	if _, err := Open(p, 1<<20); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(p); err != nil || !st.IsDir() {
		t.Fatalf("not created: %v", err)
	}
}

func TestSweepRemovesOnlyOldEntries(t *testing.T) {
	d, _ := Open(t.TempDir(), 1<<20)
	oldF := filepath.Join(d.Path(), "old")
	newF := filepath.Join(d.Path(), "new")
	oldDir := filepath.Join(d.Path(), "olddir")
	os.WriteFile(oldF, []byte("x"), 0o600)
	os.WriteFile(newF, []byte("x"), 0o600)
	os.MkdirAll(oldDir, 0o700)
	os.WriteFile(filepath.Join(oldDir, "inner"), []byte("x"), 0o600)
	past := time.Now().Add(-11 * time.Minute)
	os.Chtimes(oldF, past, past)
	os.Chtimes(oldDir, past, past)

	n := d.Sweep(10*time.Minute, time.Now())
	if n != 2 {
		t.Fatalf("swept %d, want 2", n)
	}
	if _, err := os.Stat(oldF); !os.IsNotExist(err) {
		t.Fatal("old file still there")
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatal("old dir still there")
	}
	if _, err := os.Stat(newF); err != nil {
		t.Fatal("new file was removed")
	}
}

func TestSweeperRunsOnATicker(t *testing.T) {
	d, _ := Open(t.TempDir(), 1<<20)
	f := filepath.Join(d.Path(), "old")
	os.WriteFile(f, []byte("x"), 0o600)
	past := time.Now().Add(-time.Hour)
	os.Chtimes(f, past, past)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.RunSweeper(ctx, 10*time.Millisecond, time.Minute)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(f); os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("sweeper did not remove the old file")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestReserveWaitsForBudgetAndGivesUp(t *testing.T) {
	d, _ := Open(t.TempDir(), 100)
	lease, err := d.Reserve(context.Background(), 80)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := d.Reserve(ctx, 40); err == nil {
		t.Fatal("want a timeout while the budget is used up")
	}
	lease.Release()
	l2, err := d.Reserve(context.Background(), 40)
	if err != nil {
		t.Fatal(err)
	}
	l2.Release()
}

func TestNewFileLivesInScratch(t *testing.T) {
	d, _ := Open(t.TempDir(), 1<<20)
	f, err := d.NewFile("src-*")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if filepath.Dir(f.Name()) != d.Path() {
		t.Fatalf("%s is outside %s", f.Name(), d.Path())
	}
}

func TestEnvPointsToolsAtScratch(t *testing.T) {
	d, _ := Open(t.TempDir(), 1<<20)
	env := map[string]string{}
	for _, kv := range d.Env() {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				env[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	for _, k := range []string{"TMPDIR", "MAGICK_TMPDIR"} {
		if env[k] != d.Path() {
			t.Errorf("%s = %q, want %q", k, env[k], d.Path())
		}
	}
}
