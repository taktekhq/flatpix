// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Taktek and Nizar Mahmoud

// Package origin finds and reads the original images behind /image/upload/
// URLs. Two kinds exist in v0.1: a local directory and an S3-compatible
// bucket.
package origin

import (
	"context"
	"io"
	"strings"

	"github.com/taktekhq/flatpix/internal/ferr"
)

// Info identifies one stored object.
type Info struct {
	// Key is the resolved object key, extension included. It is part of the
	// derived cache key, so two files that differ only by extension never
	// share a rendered output.
	Key  string
	ETag string // changes when the content changes
	Size int64
}

// Origin is a place originals live.
type Origin interface {
	// Stat looks an object up. A missing object is ferr.ErrNotFound.
	Stat(ctx context.Context, key string) (Info, error)
	// Download copies the object into w, reading at most max bytes. More than
	// max is ferr.ErrTooLarge and an empty object is ferr.ErrBadInput.
	Download(ctx context.Context, key string, w io.Writer, max int64) (int64, error)
	// LocalPath is the file path when the object already is a local file,
	// so no copy is needed.
	LocalPath(key string) (string, bool)
}

// extensions tried when the URL has none, or its extension does not match a
// stored file. The order is the order of preference.
var tryExts = []string{"jpg", "jpeg", "png", "webp", "gif", "avif", "tif", "tiff", "heic"}

// Resolve finds the stored object for a public id and the extension in the
// URL. Cloudinary public ids carry no extension, while files on disk or in a
// bucket usually do, so it tries, in order: the id with the URL extension,
// the id as is, then the id with each common image extension.
func Resolve(ctx context.Context, o Origin, publicID, ext string) (Info, error) {
	var candidates []string
	seen := map[string]bool{}
	add := func(k string) {
		if !seen[k] {
			seen[k] = true
			candidates = append(candidates, k)
		}
	}
	if ext != "" {
		add(publicID + "." + ext)
	}
	add(publicID)
	for _, e := range tryExts {
		add(publicID + "." + e)
	}
	for _, c := range candidates {
		info, err := o.Stat(ctx, c)
		if err == nil {
			return info, nil
		}
		if ferr.Kind(err) != ferr.ErrNotFound {
			return Info{}, err
		}
	}
	return Info{}, ferr.New(ferr.ErrNotFound, "image not found")
}

// cleanKey rejects keys that could leave the origin.
func cleanKey(key string) (string, bool) {
	if key == "" || strings.HasPrefix(key, "/") || strings.ContainsAny(key, "\x00\\") {
		return "", false
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}
	return key, true
}
