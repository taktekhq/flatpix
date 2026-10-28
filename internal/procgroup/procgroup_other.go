//go:build !unix

// Package procgroup is only implemented on Unix.
package procgroup

import (
	"context"
	"errors"
	"io"
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

// Run is not supported on this platform.
func Run(ctx context.Context, c Cmd) error { return errors.New("procgroup: unsupported platform") }

// ChildEnv returns extra unchanged.
func ChildEnv(extra, parent []string) []string { return extra }
