package transform

import (
	"errors"
	"strings"
	"testing"

	"github.com/taktekhq/flatpix/internal/cldurl"
	"github.com/taktekhq/flatpix/internal/ferr"
)

func chain(t *testing.T, url string) [][]cldurl.Param {
	t.Helper()
	r, err := cldurl.Parse("/demo/image/upload/"+url+"/x.jpg", "")
	if err != nil {
		t.Fatal(err)
	}
	return r.Chain
}

func compile(t *testing.T, url string) *Spec {
	t.Helper()
	s, err := Compile(chain(t, url), false)
	if err != nil {
		t.Fatalf("Compile(%q): %v", url, err)
	}
	return s
}

func TestNormalizeIsStable(t *testing.T) {
	// Different spellings of the same transformation share one normal form.
	same := [][]string{
		{"w_300,h_200,c_fill", "c_fill,h_200,w_300", "h_200,w_300.0,c_fill"},
		{"w_300", "c_scale,w_300", "w_300,c_scale"},
		{"w_300,f_auto", "w_300/f_auto", "f_auto,w_300"},
		{"dpr_2.0,w_100", "dpr_2,w_100", "w_100,dpr_2"},
		{"b_rgb:FF0000,c_pad,w_10,h_10", "b_rgb:ff0000,c_pad,w_10,h_10", "b_red,c_pad,w_10,h_10"},
		{"q_auto,w_10", "q_auto:good,w_10"},
		{"f_jpg,w_10", "f_jpeg,w_10"},
		{"a_270,w_10", "a_-90,w_10"},
	}
	for _, group := range same {
		want := compile(t, group[0]).Normalize()
		for _, u := range group[1:] {
			if got := compile(t, u).Normalize(); got != want {
				t.Errorf("%q normalizes to %q, want %q (same as %q)", u, got, want, group[0])
			}
		}
	}
	// Different transformations must differ.
	diff := []string{"w_300", "w_301", "h_300", "w_300,c_fill,h_100", "w_300,c_fit,h_100", "w_0.5",
		"w_300,e_grayscale", "w_300,a_90", "w_300,q_80", "w_300,f_webp", "w_300,g_north,c_fill,h_100",
		"w_300/w_100", "w_300,ar_1:1", "w_300,dpr_2"}
	seen := map[string]string{}
	for _, u := range diff {
		n := compile(t, u).Normalize()
		if prev, ok := seen[n]; ok {
			t.Errorf("%q and %q share the normal form %q", u, prev, n)
		}
		seen[n] = u
	}
}

func TestCompileRejectsWhatItCannotHonor(t *testing.T) {
	tests := []struct {
		url  string
		want string // text the message must contain
	}{
		{"x_100", "x_100"},
		{"e_sepia", "e_sepia"},
		{"fl_progressive", "fl_progressive"},
		{"w_300,zz_9", "zz_9"},
		{"c_face", "c_face"},
		{"g_face", "g_face"},
		{"w_abc", "w_abc"},
		{"w_-5", "w_-5"},
		{"w_0", "w_0"},
		{"dpr_auto", "dpr_auto"},
		{"q_200", "q_200"},
		{"q_0", "q_0"},
		{"q_auto:great", "q_auto:great"},
		{"f_tiff", "f_tiff"},
		{"a_45", "a_45"},
		{"ar_0", "ar_0"},
		{"ar_1:0", "ar_1:0"},
		{"ar_a:b", "ar_a:b"},
		{"b_notacolor", "b_notacolor"},
		{"b_rgb:12345", "b_rgb:12345"},
		{"w_70000", "w_70000"},
		{"us_east", "us_east"}, // a folder that looks like a parameter
	}
	for _, tc := range tests {
		t.Run(tc.url, func(t *testing.T) {
			_, err := Compile(chain(t, tc.url), false)
			if err == nil {
				t.Fatalf("want an error for %q", tc.url)
			}
			if !errors.Is(err, ferr.ErrBadInput) {
				t.Errorf("error kind = %v, want bad input", ferr.Kind(err))
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
		})
	}
}

func TestLenientIgnoresAndReports(t *testing.T) {
	s, err := Compile(chain(t, "w_300,e_sepia,fl_progressive,x_5"), true)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.Ignored, ","); got != "e_sepia,fl_progressive,x_5" {
		t.Errorf("Ignored = %q", got)
	}
	if s.Normalize() != compile(t, "w_300").Normalize() {
		t.Error("ignored parameters must not change the normal form")
	}
}

func TestSpecFields(t *testing.T) {
	s := compile(t, "c_fill,g_auto,w_400,h_0.5,ar_16:9,dpr_2,e_grayscale,a_90,b_rgb:00ff0080,q_auto:eco,f_auto")
	st := s.Steps[0]
	if st.Crop != "fill" || st.Gravity != "auto" || st.W.V != 400 || st.W.Rel || !st.H.Rel || st.H.V != 0.5 {
		t.Errorf("step = %+v", st)
	}
	if st.AR < 1.777 || st.AR > 1.778 || st.DPR != 2 || !st.Gray || st.Angle != 90 {
		t.Errorf("step = %+v", st)
	}
	if st.Bg == nil || st.Bg.R != 0 || st.Bg.G != 255 || st.Bg.B != 0 || st.Bg.A != 0x80 {
		t.Errorf("bg = %+v", st.Bg)
	}
	if s.Format != FormatAuto || s.Quality.Auto != "eco" {
		t.Errorf("format %q quality %+v", s.Format, s.Quality)
	}
}

func TestFormatAndQualityAreGlobalToTheChain(t *testing.T) {
	s := compile(t, "w_300/f_webp/q_70")
	if len(s.Steps) != 1 {
		t.Fatalf("steps = %d, want 1 (f_ and q_ add no step)", len(s.Steps))
	}
	if s.Format != "webp" || s.Quality.Num != 70 {
		t.Errorf("format %q quality %+v", s.Format, s.Quality)
	}
}

func TestResolveFormat(t *testing.T) {
	tests := []struct {
		f      Format
		accept string
		want   Format
	}{
		{"jpg", "image/avif", "jpg"},
		{"webp", "", "webp"},
		{FormatAuto, "image/avif,image/webp,image/apng,*/*;q=0.8", "avif"},
		{FormatAuto, "image/webp,image/apng,image/*,*/*;q=0.8", "webp"},
		{FormatAuto, "image/webp;q=0,image/avif;q=0", ""},
		{FormatAuto, "image/avif;q=0, image/webp", "webp"},
		{FormatAuto, "*/*", ""},
		{FormatAuto, "image/*", ""},
		{FormatAuto, "", ""},
		{FormatAuto, "text/html,application/xhtml+xml", ""},
		{"", "image/avif", ""},
	}
	for _, tc := range tests {
		if got := ResolveFormat(tc.f, tc.accept); got != tc.want {
			t.Errorf("ResolveFormat(%q, %q) = %q, want %q", tc.f, tc.accept, got, tc.want)
		}
	}
}

func TestQualityMapping(t *testing.T) {
	if q := (Quality{}).JPEG(); q != 90 {
		t.Errorf("default jpeg = %d", q)
	}
	if q := (Quality{Num: 70}).JPEG(); q != 70 {
		t.Errorf("jpeg = %d", q)
	}
	order := []string{"best", "good", "eco", "low"}
	prev := 101
	for _, a := range order {
		q := Quality{Auto: a}.JPEG()
		if q >= prev {
			t.Errorf("auto:%s = %d, should be lower than the previous tier %d", a, q, prev)
		}
		prev = q
	}
	if (Quality{Auto: "auto"}).JPEG() != (Quality{Auto: "good"}).JPEG() {
		t.Error("plain auto should equal auto:good")
	}
	for _, q := range []Quality{{}, {Num: 1}, {Num: 100}, {Auto: "low"}} {
		if a := q.AVIF(); a < 1 || a > 100 {
			t.Errorf("avif %+v = %d", q, a)
		}
	}
}
