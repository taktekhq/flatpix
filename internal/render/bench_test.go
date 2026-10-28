// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Taktek and Nizar Mahmoud

package render

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/davidbyttow/govips/v2/vips"
	"github.com/taktekhq/flatpix/internal/sniff"
	"github.com/taktekhq/flatpix/internal/testimg"
	"github.com/taktekhq/flatpix/internal/transform"
)

// A 4000x3000 photo-like JPEG to 800x600 WebP: the case in the README.
func benchSource(b *testing.B) string {
	b.Helper()
	p := filepath.Join(b.TempDir(), "photo.jpg")
	if err := os.WriteFile(p, testimg.JPEG(b, testimg.Patterned(4000, 3000), 90), 0o600); err != nil {
		b.Fatal(err)
	}
	return p
}

func BenchmarkRenderFillWebP(b *testing.B) {
	src := benchSource(b)
	s := spec(b, "c_fill,w_800,h_600")
	dst := filepath.Join(b.TempDir(), "out")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := (Vips{}).Render(context.Background(), Job{Src: src, SrcFormat: sniff.JPEG, Spec: s,
			OutFormat: transform.FormatWEBP, Dst: dst, Limits: testLimits}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLoadOnly(b *testing.B) {
	src := benchSource(b)
	for i := 0; i < b.N; i++ {
		img, err := vips.LoadThumbnailFromFile(src, 1600, 1200, vips.InterestingNone, vips.SizeForce, nil)
		if err != nil {
			b.Fatal(err)
		}
		img.Close()
	}
}

func BenchmarkLoadOnlyDirect(b *testing.B) {
	src := benchSource(b)
	for i := 0; i < b.N; i++ {
		img, err := vips.LoadThumbnailFromFile(src, 800, 600, vips.InterestingNone, vips.SizeForce, nil)
		if err != nil {
			b.Fatal(err)
		}
		img.Close()
	}
}

func BenchmarkInspect(b *testing.B) {
	src := benchSource(b)
	for i := 0; i < b.N; i++ {
		if _, err := inspect(src, sniff.JPEG, testLimits); err != nil {
			b.Fatal(err)
		}
	}
}
