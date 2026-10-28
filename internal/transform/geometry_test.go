package transform

import (
	"errors"
	"strings"
	"testing"

	"github.com/taktekhq/flatpix/internal/ferr"
)

const maxPx = 25_000_000

func plan(t *testing.T, url string, sw, sh int) *Plan {
	t.Helper()
	p, err := compile(t, url).Plan(sw, sh, maxPx)
	if err != nil {
		t.Fatalf("Plan(%q, %dx%d): %v", url, sw, sh, err)
	}
	return p
}

// Hand-computed expectations for a 4000x3000 source.
func TestOutputSizes(t *testing.T) {
	tests := []struct {
		url    string
		sw, sh int
		w, h   int
	}{
		// scale (the default)
		{"w_800", 4000, 3000, 800, 600},
		{"h_300", 4000, 3000, 400, 300},
		{"w_800,h_800", 4000, 3000, 800, 800},
		{"c_scale,w_5000", 4000, 3000, 5000, 3750},
		{"w_0.5", 4000, 3000, 2000, 1500},
		{"h_0.25", 4000, 3000, 1000, 750},
		{"w_1.0", 4000, 3000, 4000, 3000},
		{"w_0.5,h_0.5", 4000, 3000, 2000, 1500},
		{"dpr_2,w_400", 4000, 3000, 800, 600},
		{"dpr_1.5,w_400,h_200", 4000, 3000, 600, 300},
		{"e_grayscale", 4000, 3000, 4000, 3000},
		{"", 4000, 3000, 4000, 3000},
		// fit, limit, mfit
		{"c_fit,w_800,h_800", 4000, 3000, 800, 600},
		{"c_fit,w_5000,h_5000", 4000, 3000, 5000, 3750},
		{"c_fit,w_800", 4000, 3000, 800, 600},
		{"c_fit,h_300", 4000, 3000, 400, 300},
		{"c_limit,w_800,h_800", 4000, 3000, 800, 600},
		{"c_limit,w_5000,h_5000", 4000, 3000, 4000, 3000},
		{"c_limit,w_5000", 4000, 3000, 4000, 3000},
		{"c_mfit,w_5000,h_5000", 4000, 3000, 5000, 3750},
		{"c_mfit,w_800,h_800", 4000, 3000, 4000, 3000},
		// fill and thumb
		{"c_fill,w_800,h_800", 4000, 3000, 800, 800},
		{"c_fill,w_1600,h_900", 4000, 3000, 1600, 900},
		{"c_fill,w_5000,h_5000", 4000, 3000, 5000, 5000},
		{"c_fill,w_800", 4000, 3000, 800, 600},
		{"c_thumb,w_200,h_200", 4000, 3000, 200, 200},
		{"c_lfill,w_800,h_800", 4000, 3000, 800, 800},
		{"c_lfill,w_5000,h_5000", 4000, 3000, 4000, 3000},
		{"c_lfill,w_5000,h_1000", 4000, 3000, 4000, 1000},
		// pad
		{"c_pad,w_800,h_800", 4000, 3000, 800, 800},
		{"c_pad,w_5000,h_5000", 4000, 3000, 5000, 5000},
		{"c_lpad,w_5000,h_5000", 4000, 3000, 5000, 5000},
		{"c_lpad,w_800,h_800", 4000, 3000, 800, 800},
		// crop
		{"c_crop,w_1000,h_1000", 4000, 3000, 1000, 1000},
		{"c_crop,w_5000,h_100", 4000, 3000, 4000, 100},
		{"c_crop,w_1000", 4000, 3000, 1000, 3000},
		// aspect ratio
		{"ar_16:9,w_800", 4000, 3000, 800, 450},
		{"ar_1.5,h_300", 4000, 3000, 450, 300},
		{"ar_1:1,c_fill", 4000, 3000, 3000, 3000},
		{"ar_2:1,c_fill", 4000, 3000, 4000, 2000},
		{"ar_1:2,c_fill", 4000, 3000, 1500, 3000},
		{"ar_16:9,c_fill,w_800,h_800", 4000, 3000, 800, 800}, // both given: ar ignored
		// rotation swaps the final size
		{"w_800,a_90", 4000, 3000, 600, 800},
		{"w_800,a_180", 4000, 3000, 800, 600},
		{"w_800,a_270", 4000, 3000, 600, 800},
		{"w_800,a_auto", 4000, 3000, 800, 600},
		// chains work on the previous output
		{"c_fill,w_800,h_800/c_scale,w_400", 4000, 3000, 400, 400},
		{"c_fit,w_1000,h_1000/c_pad,w_1200,h_1200", 4000, 3000, 1200, 1200},
		{"w_1000/a_90", 4000, 3000, 750, 1000},
		// tiny and odd sources
		{"w_100", 1, 1, 100, 100},
		{"w_1", 4000, 1, 1, 1},
		{"c_fit,w_100,h_100", 3, 7, 43, 100},
		{"c_fill,w_10,h_10", 3, 7, 10, 10},
	}
	for _, tc := range tests {
		name := tc.url
		if name == "" {
			name = "(none)"
		}
		t.Run(name, func(t *testing.T) {
			var p *Plan
			if tc.url == "" {
				var err error
				p, err = (&Spec{}).Plan(tc.sw, tc.sh, maxPx)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				p = plan(t, tc.url, tc.sw, tc.sh)
			}
			if p.OutW != tc.w || p.OutH != tc.h {
				t.Errorf("out = %dx%d, want %dx%d", p.OutW, p.OutH, tc.w, tc.h)
			}
		})
	}
}

func TestFillGeometry(t *testing.T) {
	g := plan(t, "c_fill,w_800,h_800", 4000, 3000).Steps[0]
	// Cover scale is 800/3000, so the 4000x3000 source becomes 1067x800.
	if g.ResizeW != 1067 || g.ResizeH != 800 {
		t.Errorf("resize = %dx%d, want 1067x800", g.ResizeW, g.ResizeH)
	}
	if g.CropW != 800 || g.CropH != 800 || g.CropX != 133 || g.CropY != 0 {
		t.Errorf("crop = %+v", g)
	}
	for _, tc := range []struct {
		g    string
		x, y int
	}{
		{"east", 267, 0}, {"west", 0, 0}, {"north", 133, 0}, {"south", 133, 0},
		{"north_east", 267, 0}, {"north_west", 0, 0}, {"south_east", 267, 0}, {"south_west", 0, 0},
		{"center", 133, 0},
	} {
		s := plan(t, "c_fill,w_800,h_800,g_"+tc.g, 4000, 3000).Steps[0]
		if s.CropX != tc.x || s.CropY != tc.y {
			t.Errorf("g_%s: crop at %d,%d want %d,%d", tc.g, s.CropX, s.CropY, tc.x, tc.y)
		}
	}
	// Tall source: gravity moves the window vertically.
	for _, tc := range []struct {
		g string
		y int
	}{{"north", 0}, {"south", 267}, {"center", 133}} {
		s := plan(t, "c_fill,w_800,h_800,g_"+tc.g, 3000, 4000).Steps[0]
		if s.CropY != tc.y || s.CropX != 0 {
			t.Errorf("tall g_%s: crop at %d,%d want 0,%d", tc.g, s.CropX, s.CropY, tc.y)
		}
	}
	a := plan(t, "c_fill,w_800,h_800,g_auto", 4000, 3000).Steps[0]
	if a.Interest != "attention" {
		t.Errorf("g_auto interest = %q, want attention", a.Interest)
	}
	e := plan(t, "c_fill,w_800,h_800,g_auto:entropy", 4000, 3000).Steps[0]
	if e.Interest != "entropy" {
		t.Errorf("g_auto:entropy interest = %q", e.Interest)
	}
}

func TestPadGeometry(t *testing.T) {
	g := plan(t, "c_pad,w_800,h_800", 4000, 3000).Steps[0]
	if g.ResizeW != 800 || g.ResizeH != 600 || g.CanvasW != 800 || g.CanvasH != 800 || g.PadX != 0 || g.PadY != 100 {
		t.Errorf("pad = %+v", g)
	}
	g = plan(t, "c_pad,w_800,h_800,g_south", 4000, 3000).Steps[0]
	if g.PadY != 200 {
		t.Errorf("g_south PadY = %d, want 200", g.PadY)
	}
	g = plan(t, "c_lpad,w_5000,h_5000", 4000, 3000).Steps[0]
	if g.ResizeW != 0 || g.PadX != 500 || g.PadY != 1000 { // not resized: it already fits
		t.Errorf("lpad = %+v", g)
	}
}

func TestCropGeometry(t *testing.T) {
	g := plan(t, "c_crop,w_1000,h_1000", 4000, 3000).Steps[0]
	if g.ResizeW != 0 || g.CropW != 1000 || g.CropH != 1000 || g.CropX != 1500 || g.CropY != 1000 {
		t.Errorf("crop = %+v", g)
	}
	g = plan(t, "c_crop,w_1000,h_1000,g_north_west", 4000, 3000).Steps[0]
	if g.CropX != 0 || g.CropY != 0 {
		t.Errorf("crop nw = %+v", g)
	}
}

func TestLimitsAndAreaBudget(t *testing.T) {
	// An explicit size over the budget is refused.
	_, err := compile(t, "w_9000,h_9000").Plan(4000, 3000, maxPx)
	if !errors.Is(err, ferr.ErrBadInput) || !strings.Contains(err.Error(), "pixels") {
		t.Errorf("err = %v, want a bad input error about pixels", err)
	}
	// dpr counts: 4000 x dpr 5 is explicit and too big.
	_, err = compile(t, "w_4000,h_4000,dpr_5").Plan(4000, 3000, maxPx)
	if !errors.Is(err, ferr.ErrBadInput) {
		t.Errorf("err = %v", err)
	}
	// No size asked, source over the budget: size from the area, keep the ratio.
	p, err := (&Spec{}).Plan(20000, 2000, maxPx)
	if err != nil {
		t.Fatal(err)
	}
	if int64(p.OutW)*int64(p.OutH) > maxPx || int64(p.OutW)*int64(p.OutH) < maxPx*98/100 {
		t.Errorf("out %dx%d = %d px, want just under %d", p.OutW, p.OutH, p.OutW*p.OutH, maxPx)
	}
	if r := float64(p.OutW) / float64(p.OutH); r < 9.99 || r > 10.01 {
		t.Errorf("aspect ratio changed: %v", r)
	}
	// A long edge cap would have shrunk this wide image to 5000x500.
	if p.OutW < 10000 {
		t.Errorf("wide image shrunk too much: %dx%d", p.OutW, p.OutH)
	}
}

func TestFitArea(t *testing.T) {
	tests := []struct {
		w, h   int
		budget int64
	}{
		{20000, 2000, 25_000_000},
		{6000, 6000, 25_000_000},
		{30000, 30000, 1_000_000},
		{9999, 1, 5000},
		{1, 9999, 5000},
	}
	for _, tc := range tests {
		nw, nh := FitArea(tc.w, tc.h, tc.budget)
		if nw < 1 || nh < 1 {
			t.Errorf("FitArea(%d,%d) = %dx%d", tc.w, tc.h, nw, nh)
		}
		if int64(nw)*int64(nh) > tc.budget {
			t.Errorf("FitArea(%d,%d,%d) = %dx%d = %d px, over budget", tc.w, tc.h, tc.budget, nw, nh, nw*nh)
		}
	}
	if w, h := FitArea(100, 100, 25_000_000); w != 100 || h != 100 {
		t.Errorf("under budget must not change: %dx%d", w, h)
	}
	// Square and wide images get the same area.
	sw, sh := FitArea(10000, 10000, 1_000_000)
	ww, wh := FitArea(40000, 2500, 1_000_000)
	sa, wa := sw*sh, ww*wh
	if d := sa - wa; d > 20000 || d < -20000 {
		t.Errorf("square got %d px, wide got %d px", sa, wa)
	}
}

func TestPlanReportsOrientationFlags(t *testing.T) {
	p := plan(t, "e_grayscale,b_rgb:ff0000,c_pad,w_100,h_100", 400, 300)
	st := p.Steps[0]
	if !st.Gray || st.Bg == nil || st.Bg.R != 255 {
		t.Errorf("step = %+v", st)
	}
}
