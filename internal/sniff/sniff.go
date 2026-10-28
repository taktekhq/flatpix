// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Taktek and Nizar Mahmoud

// Package sniff identifies a file by its magic bytes. The extension is only
// a hint and never overrides what the bytes say.
package sniff

import "bytes"

// Format is a recognized file format.
type Format string

const (
	Unknown Format = ""
	JPEG    Format = "jpeg"
	PNG     Format = "png"
	GIF     Format = "gif"
	WEBP    Format = "webp"
	TIFF    Format = "tiff"
	AVIF    Format = "avif"
	HEIC    Format = "heic"
	PDF     Format = "pdf"
	PS      Format = "ps"
	EPS     Format = "eps"
	PSD     Format = "psd"
)

// Class says which render path handles a format.
type Class int

const (
	// Rejected formats are refused.
	Rejected Class = iota
	// Web formats are rendered in process by libvips: the fast path.
	Web
	// Print formats are rendered in a child process (v0.2).
	Print
)

// Class returns the render path for f.
func (f Format) Class() Class {
	switch f {
	case JPEG, PNG, GIF, WEBP, TIFF, AVIF, HEIC:
		return Web
	case PDF, PS, EPS, PSD:
		return Print
	}
	return Rejected
}

// ContentType is the MIME type of f.
func (f Format) ContentType() string {
	switch f {
	case JPEG:
		return "image/jpeg"
	case PNG:
		return "image/png"
	case GIF:
		return "image/gif"
	case WEBP:
		return "image/webp"
	case TIFF:
		return "image/tiff"
	case AVIF:
		return "image/avif"
	case HEIC:
		return "image/heic"
	case PDF:
		return "application/pdf"
	case PS, EPS:
		return "application/postscript"
	case PSD:
		return "image/vnd.adobe.photoshop"
	}
	return "application/octet-stream"
}

// Ext is the file extension for f, without the dot.
func (f Format) Ext() string {
	switch f {
	case JPEG:
		return "jpg"
	case HEIC:
		return "heic"
	}
	return string(f)
}

// HeadSize is how many leading bytes Sniff needs.
const HeadSize = 512

var (
	bom       = []byte{0xEF, 0xBB, 0xBF}
	dosEPS    = []byte{0xC5, 0xD0, 0xD3, 0xC6}
	heicBrand = map[string]bool{
		"heic": true, "heix": true, "hevc": true, "hevx": true, "heim": true,
		"heis": true, "hevm": true, "hevs": true, "mif1": true, "msf1": true,
	}
)

// Sniff identifies the format from the first bytes of a file.
func Sniff(head []byte) Format {
	switch {
	case bytes.HasPrefix(head, []byte{0xFF, 0xD8, 0xFF}):
		return JPEG
	case bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		return PNG
	case bytes.HasPrefix(head, []byte("GIF87a")), bytes.HasPrefix(head, []byte("GIF89a")):
		return GIF
	case len(head) >= 12 && bytes.HasPrefix(head, []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WEBP")):
		return WEBP
	case bytes.HasPrefix(head, []byte("II*\x00")), bytes.HasPrefix(head, []byte("MM\x00*")),
		bytes.HasPrefix(head, []byte("II+\x00")), bytes.HasPrefix(head, []byte("MM\x00+")):
		return TIFF
	case bytes.HasPrefix(head, []byte("8BPS")):
		return PSD
	case bytes.HasPrefix(head, dosEPS):
		return EPS
	case len(head) >= 12 && bytes.Equal(head[4:8], []byte("ftyp")):
		brand := string(head[8:12])
		if brand == "avif" || brand == "avis" {
			return AVIF
		}
		if heicBrand[brand] {
			return HEIC
		}
		return Unknown
	}
	// PDF and PostScript may start with a BOM and whitespace.
	t := bytes.TrimPrefix(head, bom)
	t = bytes.TrimLeft(t, " \t\r\n\f\x00")
	switch {
	case bytes.HasPrefix(t, []byte("%PDF-")):
		return PDF
	case bytes.HasPrefix(t, []byte("%!")):
		line := t
		if i := bytes.IndexAny(t, "\r\n"); i >= 0 {
			line = t[:i]
		}
		if bytes.Contains(line, []byte("EPSF")) {
			return EPS
		}
		return PS
	}
	return Unknown
}

// SniffWithHint is Sniff. The extension hint is accepted so call sites state
// that they have one, but it never changes the answer.
func SniffWithHint(head []byte, extHint string) Format {
	_ = extHint
	return Sniff(head)
}
