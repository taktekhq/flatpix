// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Taktek and Nizar Mahmoud

package render

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/taktekhq/flatpix/internal/ferr"
	"github.com/taktekhq/flatpix/internal/procgroup"
)

// Child renders in a child process of the same binary. The child runs in its
// own process group, and the whole group is killed on cancel or timeout, so
// no Ghostscript or ImageMagick helper outlives its render. Every tool is
// pinned to one thread.
//
// In v0.1 only the web path exists, so Child is used when isolation is turned
// on (FLATPIX_ISOLATE=1) and by the tests. In v0.2 the print path uses it.
type Child struct {
	// Exe is the binary to re-execute, normally os.Executable().
	Exe string
	// Env is the scratch directory variables (TMPDIR, MAGICK_TMPDIR).
	Env []string
}

type childOut struct {
	Result *Result    `json:"result,omitempty"`
	Error  *childFail `json:"error,omitempty"`
}

type childFail struct {
	Kind string `json:"kind"`
	Msg  string `json:"msg"`
}

var kinds = map[string]error{
	"bad_input": ferr.ErrBadInput, "not_found": ferr.ErrNotFound, "too_large": ferr.ErrTooLarge,
	"busy": ferr.ErrBusy, "upstream_failed": ferr.ErrUpstream, "internal_error": ferr.ErrInternal,
}

// Render implements Renderer.
func (c Child) Render(ctx context.Context, j Job) (*Result, error) {
	in, err := json.Marshal(j)
	if err != nil {
		return nil, ferr.Wrap(ferr.ErrInternal, err, "could not encode the job")
	}
	var stdout, stderr bytes.Buffer
	err = procgroup.Run(ctx, procgroup.Cmd{
		Path:   c.Exe,
		Env:    procgroup.ChildEnv(c.Env, os.Environ()),
		Stdin:  bytes.NewReader(in),
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var out childOut
	if jerr := json.Unmarshal(stdout.Bytes(), &out); jerr != nil {
		// No answer: the child crashed or was killed from outside.
		return nil, ferr.Wrap(ferr.ErrInternal, errors.Join(err, fmt.Errorf("stderr: %.200s", stderr.String())), "the render process died")
	}
	if out.Error != nil {
		k := kinds[out.Error.Kind]
		if k == nil {
			k = ferr.ErrInternal
		}
		return nil, ferr.New(k, "%s", out.Error.Msg)
	}
	if out.Result == nil {
		return nil, ferr.New(ferr.ErrInternal, "the render process returned nothing")
	}
	return out.Result, nil
}

// RunChild is the entry point of the render child. main calls it when
// FLATPIX_CHILD=render is set. It reads one Job from stdin, renders it with
// libvips and writes one JSON answer to stdout. It returns the exit code.
func RunChild(stdin io.Reader, stdout io.Writer) int {
	var j Job
	if err := json.NewDecoder(stdin).Decode(&j); err != nil {
		fmt.Fprintln(os.Stderr, "flatpix child: bad job:", err)
		return 2
	}
	if err := Startup(1); err != nil {
		fmt.Fprintln(os.Stderr, "flatpix child: libvips:", err)
		return 3
	}
	defer Shutdown()
	var out childOut
	res, err := Vips{}.Render(context.Background(), j)
	if err != nil {
		out.Error = &childFail{Kind: ferr.Event(err), Msg: ferr.PublicMessage(err)}
		if ferr.Kind(err) == ferr.ErrInternal {
			fmt.Fprintln(os.Stderr, "flatpix child:", err)
		}
	} else {
		out.Result = res
	}
	if err := json.NewEncoder(stdout).Encode(out); err != nil {
		return 4
	}
	return 0
}
