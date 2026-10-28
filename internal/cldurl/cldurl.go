// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Taktek and Nizar Mahmoud

// Package cldurl parses Cloudinary-style delivery URLs.
//
//	/{cloud}/image/upload/[{transformation}/...][v{version}/]{public_id}[.{ext}]
//	/{cloud}/image/fetch/[{transformation}/...]{url}
//
// The parser only splits the URL into its parts. It does not decide which
// transformation parameters are supported; the transform package does that.
package cldurl

import (
	"fmt"
	"regexp"
	"strings"
)

// Kind is the delivery type in the URL.
type Kind string

const (
	Upload Kind = "upload"
	Fetch  Kind = "fetch"
)

// Param is one transformation parameter. "w_300" is Key "w", Value "300".
type Param struct {
	Key   string
	Value string
}

// String renders the parameter the way it appears in a URL.
func (p Param) String() string { return p.Key + "_" + p.Value }

// Request is a parsed delivery URL.
type Request struct {
	Cloud string
	Kind  Kind
	// Chain holds one entry per "/" separated transformation component.
	// Each component holds its "," separated parameters in URL order.
	Chain [][]Param
	// Version is the digits of a v{version} segment, or "".
	Version string
	// PublicID is the upload public id without its extension.
	PublicID string
	// Ext is the lower-case extension of the public id, or "".
	Ext string
	// FetchURL is the remote URL for the fetch delivery type.
	FetchURL string
	// Signature is the value of an s--value-- segment. It is parsed and
	// ignored in v0.1.
	Signature string
}

// Source returns the requested source path as written in the URL,
// including its extension. For fetch it is the remote URL.
func (r *Request) Source() string {
	if r.Kind == Fetch {
		return r.FetchURL
	}
	if r.Ext != "" {
		return r.PublicID + "." + r.Ext
	}
	return r.PublicID
}

// ParseError is a URL that cannot be served. Status is the HTTP status.
type ParseError struct {
	Status int
	Msg    string
}

func (e *ParseError) Error() string { return e.Msg }

func bad(format string, a ...any) error {
	return &ParseError{Status: 400, Msg: fmt.Sprintf(format, a...)}
}

var (
	cloudRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	versionRe = regexp.MustCompile(`^v[0-9]+$`)
	sigRe     = regexp.MustCompile(`^s--([A-Za-z0-9_-]+)--$`)
	// A transformation component starts with a key of 1 to 3 letters (or a
	// $variable name) followed by an underscore.
	keyRe    = regexp.MustCompile(`^(\$[A-Za-z0-9]+|[a-z]{1,3})$`)
	schemeRe = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*):`)
	httpRe   = regexp.MustCompile(`^(?i)(https?):/+`)
)

// knownExts are extensions that are split off the public id.
var knownExts = map[string]bool{
	"jpg": true, "jpeg": true, "png": true, "gif": true, "webp": true, "avif": true,
	"tif": true, "tiff": true, "heic": true, "heif": true, "bmp": true,
	"pdf": true, "ai": true, "eps": true, "ps": true, "psd": true,
}

// Parse splits a decoded URL path and raw query into a Request.
func Parse(urlPath, rawQuery string) (*Request, error) {
	if strings.ContainsRune(urlPath, 0) || strings.ContainsRune(rawQuery, 0) {
		return nil, bad("invalid path: contains a NUL byte")
	}
	p := strings.TrimPrefix(urlPath, "/")
	parts := strings.SplitN(p, "/", 4)
	if len(parts) < 3 || parts[0] == "" {
		return nil, &ParseError{Status: 404, Msg: "not found"}
	}
	cloud, resource, typ := parts[0], parts[1], parts[2]
	rest := ""
	if len(parts) == 4 {
		rest = parts[3]
	}
	if resource != "image" {
		if resource == "video" || resource == "raw" {
			return nil, bad("only image/upload and image/fetch URLs are supported, got %s/%s", resource, typ)
		}
		return nil, &ParseError{Status: 404, Msg: "not found"}
	}
	if !cloudRe.MatchString(cloud) {
		return nil, bad("invalid cloud name %q", cloud)
	}
	req := &Request{Cloud: cloud}
	switch typ {
	case "upload":
		req.Kind = Upload
		if err := parseUpload(req, rest); err != nil {
			return nil, err
		}
	case "fetch":
		req.Kind = Fetch
		if err := parseFetch(req, rest, rawQuery); err != nil {
			return nil, err
		}
	default:
		return nil, bad("delivery type %q is not supported, only image/upload and image/fetch are", typ)
	}
	return req, nil
}

// looksLikeTransform reports whether a path segment has the shape of a
// transformation component: its first parameter is key_value.
func looksLikeTransform(seg string) bool {
	first, _, _ := strings.Cut(seg, ",")
	i := strings.IndexByte(first, '_')
	return i > 0 && keyRe.MatchString(first[:i])
}

func parseComponent(seg string) ([]Param, error) {
	var out []Param
	for _, part := range strings.Split(seg, ",") {
		if part == "" {
			return nil, bad("empty parameter in transformation %q", seg)
		}
		i := strings.IndexByte(part, '_')
		if i <= 0 || i == len(part)-1 {
			return nil, bad("parameter %q needs a value, as in w_300", part)
		}
		out = append(out, Param{Key: part[:i], Value: part[i+1:]})
	}
	return out, nil
}

func parseUpload(req *Request, rest string) error {
	segs := strings.Split(rest, "/")
	if m := sigRe.FindStringSubmatch(segs[0]); m != nil && len(segs) > 1 {
		req.Signature = m[1]
		segs = segs[1:]
	}
	// Find a version segment. It needs at least one segment after it.
	vi := -1
	for i := 0; i < len(segs)-1; i++ {
		if versionRe.MatchString(segs[i]) {
			vi = i
			break
		}
	}
	var transforms, idSegs []string
	if vi >= 0 {
		for _, s := range segs[:vi] {
			if !looksLikeTransform(s) {
				return bad("%q before the version is not a transformation; put folders after the v%s segment",
					s, segs[vi][1:])
			}
		}
		transforms = segs[:vi]
		req.Version = segs[vi][1:]
		idSegs = segs[vi+1:]
	} else {
		i := 0
		for i < len(segs)-1 && looksLikeTransform(segs[i]) {
			i++
		}
		transforms, idSegs = segs[:i], segs[i:]
	}
	for _, s := range transforms {
		comp, err := parseComponent(s)
		if err != nil {
			return err
		}
		req.Chain = append(req.Chain, comp)
	}
	id := strings.Join(idSegs, "/")
	if id == "" || idSegs[len(idSegs)-1] == "" {
		return bad("missing public id")
	}
	for _, s := range idSegs {
		if s == "" || s == "." || s == ".." || strings.Contains(s, `\`) {
			return bad("invalid public id %q", id)
		}
	}
	if i := strings.LastIndexByte(id, '.'); i > 0 && !strings.HasSuffix(id[:i], "/") {
		ext := strings.ToLower(id[i+1:])
		if knownExts[ext] {
			req.Ext = ext
			id = id[:i]
		}
	}
	req.PublicID = id
	return nil
}

func parseFetch(req *Request, rest, rawQuery string) error {
	segs := strings.Split(rest, "/")
	i := 0
	for i < len(segs) && looksLikeTransform(segs[i]) {
		comp, err := parseComponent(segs[i])
		if err != nil {
			return err
		}
		req.Chain = append(req.Chain, comp)
		i++
	}
	u := strings.Join(segs[i:], "/")
	if u == "" {
		return bad("missing url to fetch")
	}
	if m := httpRe.FindStringSubmatch(u); m != nil {
		u = strings.ToLower(m[1]) + "://" + u[len(m[0]):]
	} else if m := schemeRe.FindStringSubmatch(u); m != nil {
		return bad("fetch url scheme %q is not supported, use http or https", m[1])
	} else {
		return bad("fetch url must start with http:// or https://")
	}
	if q := stripAnalytics(rawQuery); q != "" {
		u += "?" + q
	}
	req.FetchURL = u
	return nil
}

// stripAnalytics removes the _a=... parameter that Cloudinary SDKs append.
func stripAnalytics(raw string) string {
	if raw == "" {
		return ""
	}
	var keep []string
	for _, kv := range strings.Split(raw, "&") {
		if kv == "" || kv == "_a" || strings.HasPrefix(kv, "_a=") {
			continue
		}
		keep = append(keep, kv)
	}
	return strings.Join(keep, "&")
}
