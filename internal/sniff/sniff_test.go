// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Taktek and Nizar Mahmoud

package sniff

import "testing"

func TestSniff(t *testing.T) {
	tests := []struct {
		name string
		head []byte
		want Format
	}{
		{"jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F'}, JPEG},
		{"jpeg exif", []byte{0xFF, 0xD8, 0xFF, 0xE1}, JPEG},
		{"png", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), PNG},
		{"gif87", []byte("GIF87a\x01\x00"), GIF},
		{"gif89", []byte("GIF89a\x01\x00"), GIF},
		{"webp", []byte("RIFF\x24\x00\x00\x00WEBPVP8 "), WEBP},
		{"riff but not webp", []byte("RIFF\x24\x00\x00\x00WAVEfmt "), Unknown},
		{"tiff little endian", []byte("II*\x00\x08\x00\x00\x00"), TIFF},
		{"tiff big endian", []byte("MM\x00*\x00\x00\x00\x08"), TIFF},
		{"bigtiff", []byte("II+\x00\x08\x00\x00\x00"), TIFF},
		{"avif", []byte("\x00\x00\x00\x1cftypavif\x00\x00\x00\x00"), AVIF},
		{"avif sequence", []byte("\x00\x00\x00\x1cftypavis\x00\x00\x00\x00"), AVIF},
		{"heic", []byte("\x00\x00\x00\x18ftypheic\x00\x00\x00\x00"), HEIC},
		{"heif mif1", []byte("\x00\x00\x00\x18ftypmif1\x00\x00\x00\x00"), HEIC},
		{"pdf", []byte("%PDF-1.7\n%\xe2\xe3"), PDF},
		{"pdf after whitespace", []byte("\n \r\n%PDF-1.4"), PDF},
		{"postscript", []byte("%!PS-Adobe-3.0\n%%Title: x"), PS},
		{"postscript after bom and whitespace", []byte("\xEF\xBB\xBF \r\n\t%!PS-Adobe-3.0\n"), PS},
		{"eps", []byte("%!PS-Adobe-3.0 EPSF-3.0\n%%BoundingBox: 0 0 10 10"), EPS},
		{"eps after bom", []byte("\xEF\xBB\xBF%!PS-Adobe-3.0 EPSF-3.0\n"), EPS},
		{"dos eps binary header", []byte{0xC5, 0xD0, 0xD3, 0xC6, 0x1E, 0, 0, 0}, EPS},
		{"psd", []byte("8BPS\x00\x01\x00\x00\x00\x00\x00\x00"), PSD},
		{"plain text", []byte("hello world, not an image"), Unknown},
		{"svg is not accepted", []byte("<svg xmlns='http://www.w3.org/2000/svg'/>"), Unknown},
		{"empty", nil, Unknown},
		{"one byte", []byte{0xFF}, Unknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Sniff(tc.head); got != tc.want {
				t.Errorf("Sniff = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClasses(t *testing.T) {
	for _, f := range []Format{JPEG, PNG, GIF, WEBP, TIFF, AVIF, HEIC} {
		if f.Class() != Web {
			t.Errorf("%s class = %v, want Web", f, f.Class())
		}
	}
	for _, f := range []Format{PDF, PS, EPS, PSD} {
		if f.Class() != Print {
			t.Errorf("%s class = %v, want Print", f, f.Class())
		}
	}
	if Unknown.Class() != Rejected {
		t.Error("unknown must be rejected")
	}
}

func TestExtensionIsOnlyAHint(t *testing.T) {
	// A PNG named .jpg is a PNG.
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	if got := SniffWithHint(png, "jpg"); got != PNG {
		t.Errorf("got %q, want png", got)
	}
	// The hint never turns unknown bytes into a format.
	if got := SniffWithHint([]byte("junk junk junk"), "jpg"); got != Unknown {
		t.Errorf("got %q, want unknown", got)
	}
}

func TestContentTypeAndExt(t *testing.T) {
	cases := map[Format][2]string{
		JPEG: {"image/jpeg", "jpg"}, PNG: {"image/png", "png"}, GIF: {"image/gif", "gif"},
		WEBP: {"image/webp", "webp"}, AVIF: {"image/avif", "avif"},
	}
	for f, want := range cases {
		if f.ContentType() != want[0] || f.Ext() != want[1] {
			t.Errorf("%s: %s %s", f, f.ContentType(), f.Ext())
		}
	}
}
