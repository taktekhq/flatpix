// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Taktek and Nizar Mahmoud

//go:build unix

// Package procgroup runs a child process in its own process group and kills
// the whole group when the context ends. Print files (v0.2) are rendered this
// way, so that Ghostscript and ImageMagick children die with the render.
package procgroup

import (
	"context"
	"io"
	"os/exec"
	"strings"
	"syscall"
)

// Cmd describes a child process.
type Cmd struct {
	Path   string
	Args   []string
	Env    []string
	Dir    string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Run starts the child in a new process group and waits for it. When ctx
// ends, it sends SIGKILL to the whole group, so grandchildren die too.
func Run(ctx context.Context, c Cmd) error {
	cmd := exec.Command(c.Path, c.Args...)
	cmd.Env = c.Env
	cmd.Dir = c.Dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = c.Stdin, c.Stdout, c.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		// Even on a normal exit, stragglers in the group must not linger.
		killGroup(cmd.Process.Pid)
		return err
	case <-ctx.Done():
		killGroup(cmd.Process.Pid)
		<-done
		return ctx.Err()
	}
}

func killGroup(pid int) {
	// A negative pid signals the whole process group.
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

// ChildEnv builds the environment for a render child. It passes only PATH
// and LANG through from the parent (never the parent's secrets), adds the
// extra variables (the scratch directory ones), and pins every tool to one
// thread. The seats already bound the parallelism.
func ChildEnv(extra, parent []string) []string {
	var env []string
	for _, kv := range parent {
		k, _, _ := strings.Cut(kv, "=")
		if k == "PATH" || k == "LANG" || k == "LC_ALL" {
			env = append(env, kv)
		}
	}
	env = append(env, extra...)
	return append(env,
		"FLATPIX_CHILD=render",
		"MAGICK_THREAD_LIMIT=1",
		"OMP_NUM_THREADS=1",
		"VIPS_CONCURRENCY=1",
		"VIPS_BLOCK_UNTRUSTED=1",
	)
}
