// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Taktek and Nizar Mahmoud

// Package render turns a source file and a transformation into an output
// file. There are two render paths:
//
//   - Web images (JPEG, PNG, WebP, AVIF, GIF, TIFF, HEIC) are rendered in
//     process by libvips. This is the fast path.
//   - Print files (PDF, AI, EPS, PS, PSD) will be rendered in a child process
//     of the same binary, in its own process group, killed as a group on
//     cancel or timeout. That path ships in v0.2; the seam (Child, RunChild
//     and the Dispatcher) is here already.
package render

import (
	"context"

	"github.com/taktekhq/flatpix/internal/ferr"
	"github.com/taktekhq/flatpix/internal/sniff"
	"github.com/taktekhq/flatpix/internal/transform"
)

// Limits bound what a render may read and write.
type Limits struct {
	MaxInputPixels  int64 // pixels in the decoded source
	MaxOutputPixels int64 // pixels in the output
}

// Job is one render. It is plain data, so it can cross a process boundary.
type Job struct {
	Src       string           // source file path
	SrcFormat sniff.Format     // from the magic bytes, never from the name
	Spec      *transform.Spec  // what to do
	OutFormat transform.Format // resolved output format; "" keeps the source format
	Dst       string           // where to write the output
	Limits    Limits
}

// Result describes the output file.
type Result struct {
	Size        int64
	ContentType string
	Format      string // "jpg", "png", "webp", "avif" or "gif"
	Width       int
	Height      int
}

// Renderer renders jobs.
type Renderer interface {
	Render(ctx context.Context, j Job) (*Result, error)
}

// Dispatcher picks the render path from the sniffed source format.
type Dispatcher struct {
	// Web renders web images. Required.
	Web Renderer
	// Print renders print files. Nil in v0.1.
	Print Renderer
}

// Render implements Renderer.
func (d *Dispatcher) Render(ctx context.Context, j Job) (*Result, error) {
	switch j.SrcFormat.Class() {
	case sniff.Web:
		return d.Web.Render(ctx, j)
	case sniff.Print:
		if d.Print == nil {
			return nil, ferr.New(ferr.ErrBadInput,
				"%s files are not supported yet: print files (PDF, AI, EPS, PS, PSD) arrive in v0.2", j.SrcFormat)
		}
		return d.Print.Render(ctx, j)
	}
	return nil, ferr.New(ferr.ErrBadInput, "the original is not a supported image (JPEG, PNG, WebP, AVIF, GIF, TIFF, HEIC)")
}
