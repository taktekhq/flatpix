package render

import (
	"bytes"
	"context"
	"errors"
	"image"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/davidbyttow/govips/v2/vips"
	"github.com/taktekhq/flatpix/internal/cldurl"
	"github.com/taktekhq/flatpix/internal/ferr"
	"github.com/taktekhq/flatpix/internal/sniff"
	"github.com/taktekhq/flatpix/internal/transform"
)

func TestMain(m *testing.M) {
	if os.Getenv("FLATPIX_CHILD") == "render" {
		os.Exit(RunChild(os.Stdin, os.Stdout))
	}
	if err := Startup(1); err != nil {
		panic(err)
	}
	code := m.Run()
	Shutdown()
	os.Exit(code)
}

var testLimits = Limits{MaxInputPixels: 100_000_000, MaxOutputPixels: 25_000_000}

func spec(t testing.TB, params string) *transform.Spec {
	t.Helper()
	var chain [][]cldurl.Param
	if params != "" {
		r, err := cldurl.Parse("/demo/image/upload/"+params+"/x.jpg", "")
		if err != nil {
			t.Fatal(err)
		}
		chain = r.Chain
	}
	s, err := transform.Compile(chain, false)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// write stores fixture bytes under a deliberately misleading name.
func write(t testing.TB, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// run renders data with the given parameters and returns the result and the
// path of the output file.
func run(t testing.TB, data []byte, params string, out transform.Format) (*Result, string, error) {
	t.Helper()
	src := write(t, "photo.jpg", data) // the name says jpg; the bytes decide
	dst := filepath.Join(t.TempDir(), "out")
	res, err := Vips{}.Render(context.Background(), Job{
		Src: src, SrcFormat: sniff.Sniff(data), Spec: spec(t, params), OutFormat: out, Dst: dst, Limits: testLimits,
	})
	return res, dst, err
}

func mustRun(t testing.TB, data []byte, params string, out transform.Format) (*Result, string) {
	t.Helper()
	res, dst, err := run(t, data, params, out)
	if err != nil {
		t.Fatalf("render %q: %v", params, err)
	}
	return res, dst
}

// pixel reads one pixel of an output file as RGBA.
func pixel(t testing.TB, path string, x, y int) [4]float64 {
	t.Helper()
	img, err := vips.NewImageFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer img.Close()
	if err := img.ToColorSpace(vips.InterpretationSRGB); err != nil {
		t.Fatal(err)
	}
	if !img.HasAlpha() {
		if err := img.AddAlpha(); err != nil {
			t.Fatal(err)
		}
	}
	v, err := img.GetPoint(x, y)
	if err != nil {
		t.Fatal(err)
	}
	return [4]float64{v[0], v[1], v[2], v[3]}
}

func near(a, b float64) bool { return a-b < 40 && b-a < 40 }

func isRed(p [4]float64) bool  { return near(p[0], 255) && near(p[1], 0) && near(p[2], 0) }
func isBlue(p [4]float64) bool { return near(p[0], 0) && near(p[1], 0) && near(p[2], 255) }

func dims(t testing.TB, path string) (int, int) {
	t.Helper()
	img, err := vips.NewImageFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer img.Close()
	return img.Width(), img.Height()
}

func TestOutputMatchesThePlan(t *testing.T) {
	src := encodeJPEG(t, patterned(400, 300), 90)
	tests := []struct {
		params string
		w, h   int
	}{
		{"", 400, 300},
		{"w_200", 200, 150},
		{"h_100", 133, 100},
		{"w_200,h_200", 200, 200},
		{"c_fit,w_100,h_100", 100, 75},
		{"c_limit,w_1000,h_1000", 400, 300},
		{"c_mfit,w_1000,h_1000", 1000, 750},
		{"c_fill,w_100,h_100", 100, 100},
		{"c_fill,w_100,h_100,g_auto", 100, 100},
		{"c_fill,w_100,h_100,g_auto:entropy", 100, 100},
		{"c_fill,w_100,h_100,g_north_east", 100, 100},
		{"c_lfill,w_1000,h_100", 400, 100},
		{"c_thumb,w_64,h_64", 64, 64},
		{"c_pad,w_200,h_200", 200, 200},
		{"c_lpad,w_600,h_600", 600, 600},
		{"c_crop,w_100,h_100", 100, 100},
		{"c_crop,w_1000,h_50", 400, 50},
		{"ar_16:9,w_320", 320, 180},
		{"w_0.5", 200, 150},
		{"dpr_2,w_100", 200, 150},
		{"w_200,a_90", 150, 200},
		{"w_200,a_180", 200, 150},
		{"c_fill,w_200,h_200/c_scale,w_50", 50, 50},
		{"c_scale,w_300/c_fill,w_100,h_100,g_auto", 100, 100},
		{"c_crop,w_150,h_150,g_auto", 150, 150},
		{"c_lfill,w_1000,h_100,g_auto", 400, 100},
		{"c_fit,w_300,h_300/c_pad,w_320,h_320", 320, 320},
		{"w_200/a_270", 150, 200},
	}
	for _, tc := range tests {
		t.Run(tc.params, func(t *testing.T) {
			res, dst := mustRun(t, src, tc.params, transform.FormatPNG)
			w, h := dims(t, dst)
			if w != tc.w || h != tc.h || res.Width != tc.w || res.Height != tc.h {
				t.Errorf("output %dx%d (result %dx%d), want %dx%d", w, h, res.Width, res.Height, tc.w, tc.h)
			}
		})
	}
}

func TestFormats(t *testing.T) {
	src := encodeJPEG(t, patterned(300, 200), 90)
	want := map[transform.Format]sniff.Format{
		transform.FormatJPG: sniff.JPEG, transform.FormatPNG: sniff.PNG, transform.FormatWEBP: sniff.WEBP,
		transform.FormatAVIF: sniff.AVIF, transform.FormatGIF: sniff.GIF,
	}
	for f, sf := range want {
		t.Run(string(f), func(t *testing.T) {
			res, dst, err := run(t, src, "w_150", f)
			if err != nil {
				if (f == transform.FormatAVIF || f == transform.FormatGIF) && errors.Is(err, ferr.ErrBadInput) {
					t.Skipf("%s output not available in this libvips build: %v", f, err)
				}
				t.Fatal(err)
			}
			b, _ := os.ReadFile(dst)
			if got := sniff.Sniff(b); got != sf {
				t.Errorf("output sniffs as %q, want %q", got, sf)
			}
			if res.ContentType != sf.ContentType() || res.Size != int64(len(b)) {
				t.Errorf("result = %+v", res)
			}
		})
	}
}

func TestOriginalFormatIsKept(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want sniff.Format
	}{
		{"jpeg", encodeJPEG(t, patterned(100, 80), 90), sniff.JPEG},
		{"png", encodePNG(t, patterned(100, 80)), sniff.PNG},
		{"gif", encodeGIF(t, patterned(100, 80)), sniff.GIF},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, dst, err := run(t, tc.data, "w_50", transform.FormatOriginal)
			if err != nil {
				if tc.want == sniff.GIF && errors.Is(err, ferr.ErrBadInput) {
					t.Skipf("gif output not available: %v", err)
				}
				t.Fatal(err)
			}
			b, _ := os.ReadFile(dst)
			if got := sniff.Sniff(b); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestGIFUsesTheFirstFrameOnly(t *testing.T) {
	_, dst, err := run(t, encodeGIF(t, split(60, 40, red, blue)), "", transform.FormatPNG)
	if err != nil {
		t.Fatal(err)
	}
	w, h := dims(t, dst)
	if w != 60 || h != 40 {
		t.Errorf("got %dx%d: frames were stacked", w, h)
	}
}

func TestAlphaIsFlattenedForJPEG(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 40, 40)) // fully transparent
	for y := 10; y < 30; y++ {
		for x := 10; x < 30; x++ {
			img.SetRGBA(x, y, red)
		}
	}
	src := encodePNG(t, img)
	_, dst := mustRun(t, src, "", transform.FormatJPG)
	if p := pixel(t, dst, 2, 2); !(near(p[0], 255) && near(p[1], 255) && near(p[2], 255)) {
		t.Errorf("corner = %v, want white", p)
	}
	_, dst = mustRun(t, src, "b_blue", transform.FormatJPG)
	if p := pixel(t, dst, 2, 2); !isBlue(p) {
		t.Errorf("corner with b_blue = %v, want blue", p)
	}
	// PNG keeps the transparency.
	_, dst = mustRun(t, src, "", transform.FormatPNG)
	if p := pixel(t, dst, 2, 2); p[3] != 0 {
		t.Errorf("png corner alpha = %v, want 0", p[3])
	}
}

func TestPadBackground(t *testing.T) {
	src := encodePNG(t, solid(200, 100, blue))
	_, dst := mustRun(t, src, "c_pad,w_100,h_100,b_rgb:ff0000", transform.FormatPNG)
	if p := pixel(t, dst, 50, 5); !isRed(p) {
		t.Errorf("padding = %v, want red", p)
	}
	if p := pixel(t, dst, 50, 50); !isBlue(p) {
		t.Errorf("image = %v, want blue", p)
	}
	// No b_ on a format with alpha: transparent padding.
	_, dst = mustRun(t, src, "c_pad,w_100,h_100", transform.FormatPNG)
	if p := pixel(t, dst, 50, 5); p[3] != 0 {
		t.Errorf("padding alpha = %v, want 0", p[3])
	}
	// No b_ on JPEG: white padding.
	_, dst = mustRun(t, src, "c_pad,w_100,h_100", transform.FormatJPG)
	if p := pixel(t, dst, 50, 5); !(near(p[0], 255) && near(p[1], 255) && near(p[2], 255)) {
		t.Errorf("jpeg padding = %v, want white", p)
	}
}

func TestGravityPicksTheWindow(t *testing.T) {
	src := encodePNG(t, split(400, 200, red, blue))
	_, dst := mustRun(t, src, "c_fill,w_100,h_100,g_west", transform.FormatPNG)
	if p := pixel(t, dst, 50, 50); !isRed(p) {
		t.Errorf("g_west centre = %v, want red", p)
	}
	_, dst = mustRun(t, src, "c_fill,w_100,h_100,g_east", transform.FormatPNG)
	if p := pixel(t, dst, 50, 50); !isBlue(p) {
		t.Errorf("g_east centre = %v, want blue", p)
	}
	_, dst = mustRun(t, src, "c_crop,w_100,h_100,g_west", transform.FormatPNG)
	if p := pixel(t, dst, 50, 50); !isRed(p) {
		t.Errorf("crop g_west = %v, want red", p)
	}
}

func TestRotationIsClockwise(t *testing.T) {
	src := encodePNG(t, split(200, 100, red, blue)) // red left, blue right
	_, dst := mustRun(t, src, "a_90", transform.FormatPNG)
	if w, h := dims(t, dst); w != 100 || h != 200 {
		t.Fatalf("got %dx%d", w, h)
	}
	if p := pixel(t, dst, 50, 10); !isRed(p) {
		t.Errorf("top after a_90 = %v, want red", p)
	}
	if p := pixel(t, dst, 50, 190); !isBlue(p) {
		t.Errorf("bottom after a_90 = %v, want blue", p)
	}
	_, dst = mustRun(t, src, "a_180", transform.FormatPNG)
	if p := pixel(t, dst, 10, 50); !isBlue(p) {
		t.Errorf("left after a_180 = %v, want blue", p)
	}
}

func TestGrayscale(t *testing.T) {
	_, dst := mustRun(t, encodePNG(t, split(40, 40, red, blue)), "e_grayscale", transform.FormatPNG)
	for _, x := range []int{5, 35} {
		p := pixel(t, dst, x, 20)
		if p[0]-p[1] > 3 || p[1]-p[0] > 3 || p[1]-p[2] > 3 || p[2]-p[1] > 3 {
			t.Errorf("pixel %d = %v is not grey", x, p)
		}
	}
	a, b := pixel(t, dst, 5, 20), pixel(t, dst, 35, 20)
	if a[0] == b[0] {
		t.Error("red and blue gave the same grey; luminance is lost")
	}
}

func TestEXIFOrientationIsApplied(t *testing.T) {
	jpg := withOrientation(t, encodeJPEG(t, patterned(100, 50), 90), 6) // needs a 90 degree turn
	_, dst := mustRun(t, jpg, "", transform.FormatPNG)
	if w, h := dims(t, dst); w != 50 || h != 100 {
		t.Errorf("no resize: got %dx%d, want 50x100", w, h)
	}
	// With a resize the first step is the shrink-on-load thumbnail path.
	_, dst = mustRun(t, jpg, "w_25", transform.FormatPNG)
	if w, h := dims(t, dst); w != 25 || h != 50 {
		t.Errorf("w_25: got %dx%d, want 25x50", w, h)
	}
	_, dst = mustRun(t, jpg, "c_fill,w_20,h_20", transform.FormatPNG)
	if w, h := dims(t, dst); w != 20 || h != 20 {
		t.Errorf("fill: got %dx%d", w, h)
	}
}

func TestHugeHeadersAreRefusedBeforeDecoding(t *testing.T) {
	for name, data := range map[string][]byte{
		"png":  hugePNGHeader(60000, 60000),
		"jpeg": hugeJPEGHeader(t, 60000, 60000),
	} {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			_, _, err := run(t, data, "w_100", transform.FormatJPG)
			if !errors.Is(err, ferr.ErrTooLarge) {
				t.Fatalf("err = %v, want too large", err)
			}
			if time.Since(start) > 2*time.Second {
				t.Errorf("took %v: the file was decoded", time.Since(start))
			}
		})
	}
}

func TestBrokenFilesAreBadInput(t *testing.T) {
	good := encodeJPEG(t, patterned(200, 200), 90)
	for name, data := range map[string][]byte{
		"magic only":       {0xFF, 0xD8, 0xFF, 0xE0, 0, 0},
		"truncated header": good[:20],
		"png header only":  encodePNG(t, patterned(10, 10))[:30],
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := run(t, data, "w_50", transform.FormatJPG)
			if !errors.Is(err, ferr.ErrBadInput) {
				t.Fatalf("err = %v (kind %v), want bad input", err, ferr.Kind(err))
			}
		})
	}
}

func TestOddBoxes(t *testing.T) {
	for _, d := range [][2]int{{1, 1}, {1, 500}, {500, 1}, {3, 7}, {2, 2}} {
		src := encodePNG(t, patterned(d[0], d[1]))
		for _, p := range []string{"w_100", "h_100", "c_fill,w_50,h_50", "c_pad,w_50,h_50", "c_fit,w_30,h_30", "c_crop,w_2,h_2", "c_lfill,w_5,h_5", "a_90", "w_1"} {
			res, dst, err := run(t, src, p, transform.FormatPNG)
			if err != nil {
				t.Errorf("%dx%d %s: %v", d[0], d[1], p, err)
				continue
			}
			if w, h := dims(t, dst); w != res.Width || h != res.Height || w < 1 || h < 1 {
				t.Errorf("%dx%d %s: file %dx%d, result %dx%d", d[0], d[1], p, w, h, res.Width, res.Height)
			}
		}
	}
}

func TestMislabeledExtensionsDoNotMatter(t *testing.T) {
	// A PNG named .jpg, a JPEG named .png, a WebP-free GIF named .webp.
	for name, data := range map[string][]byte{
		"a.jpg":  encodePNG(t, patterned(64, 64)),
		"b.png":  encodeJPEG(t, patterned(64, 64), 80),
		"c.webp": encodeGIF(t, patterned(64, 64)),
	} {
		src := write(t, name, data)
		dst := filepath.Join(t.TempDir(), "out")
		_, err := Vips{}.Render(context.Background(), Job{Src: src, SrcFormat: sniff.Sniff(data), Spec: spec(t, "w_32"),
			OutFormat: transform.FormatPNG, Dst: dst, Limits: testLimits})
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestQuality(t *testing.T) {
	src := encodeJPEG(t, patterned(600, 400), 95)
	lo, _ := mustRun(t, src, "q_20", transform.FormatJPG)
	hi, _ := mustRun(t, src, "q_95", transform.FormatJPG)
	if lo.Size >= hi.Size {
		t.Errorf("q_20 gave %d bytes, q_95 gave %d", lo.Size, hi.Size)
	}
	eco, _ := mustRun(t, src, "q_auto:eco", transform.FormatJPG)
	best, _ := mustRun(t, src, "q_auto:best", transform.FormatJPG)
	if eco.Size >= best.Size {
		t.Errorf("eco gave %d bytes, best gave %d", eco.Size, best.Size)
	}
}

func TestTIFFSourceRenders(t *testing.T) {
	ref, err := vips.NewImageFromBuffer(encodePNG(t, patterned(80, 60)))
	if err != nil {
		t.Fatal(err)
	}
	tiff, _, err := ref.ExportTiff(vips.NewTiffExportParams())
	ref.Close()
	if err != nil {
		t.Skipf("cannot write TIFF here: %v", err)
	}
	if sniff.Sniff(tiff) != sniff.TIFF {
		t.Fatal("fixture is not a TIFF")
	}
	res, _ := mustRun(t, tiff, "w_40", transform.FormatOriginal)
	if res.Format != "jpg" || res.Width != 40 {
		t.Errorf("result = %+v: a TIFF source should come out as JPEG", res)
	}
}

func TestDispatcherRefusesPrintFilesForNow(t *testing.T) {
	d := &Dispatcher{Web: Vips{}}
	for name, data := range map[string][]byte{"pdf": pdfBytes, "ps": psBytes, "eps": epsBytes, "eps-dos": epsDOS, "psd": psdBytes} {
		src := write(t, "design.jpg", data) // mislabeled on purpose
		_, err := d.Render(context.Background(), Job{Src: src, SrcFormat: sniff.Sniff(data), Spec: spec(t, "w_100"),
			Dst: filepath.Join(t.TempDir(), "o"), Limits: testLimits})
		if !errors.Is(err, ferr.ErrBadInput) {
			t.Errorf("%s: err = %v, want bad input", name, err)
		}
	}
	_, err := d.Render(context.Background(), Job{Src: write(t, "x", []byte("hello")), SrcFormat: sniff.Unknown, Spec: spec(t, ""),
		Dst: filepath.Join(t.TempDir(), "o"), Limits: testLimits})
	if !errors.Is(err, ferr.ErrBadInput) {
		t.Errorf("unknown: err = %v", err)
	}
}

func TestChildProcessGivesTheSameOutput(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	src := encodeJPEG(t, patterned(500, 400), 90)
	path := write(t, "photo.jpg", src)
	job := func(dst string) Job {
		return Job{Src: path, SrcFormat: sniff.JPEG, Spec: spec(t, "c_fill,w_120,h_90,g_auto"), OutFormat: transform.FormatWEBP,
			Dst: dst, Limits: testLimits}
	}
	inDst := filepath.Join(t.TempDir(), "in")
	inRes, err := Vips{}.Render(context.Background(), job(inDst))
	if err != nil {
		t.Fatal(err)
	}
	chDst := filepath.Join(t.TempDir(), "child")
	chRes, err := Child{Exe: exe, Env: []string{"TMPDIR=" + t.TempDir()}}.Render(context.Background(), job(chDst))
	if err != nil {
		t.Fatal(err)
	}
	if *inRes != *chRes {
		t.Errorf("in process %+v, child %+v", inRes, chRes)
	}
	a, _ := os.ReadFile(inDst)
	b, _ := os.ReadFile(chDst)
	if !bytes.Equal(a, b) {
		t.Errorf("outputs differ: %d and %d bytes", len(a), len(b))
	}
}

func TestChildProcessReportsErrorKinds(t *testing.T) {
	exe, _ := os.Executable()
	data := hugePNGHeader(60000, 60000)
	_, err := Child{Exe: exe}.Render(context.Background(), Job{Src: write(t, "x.png", data), SrcFormat: sniff.PNG,
		Spec: spec(t, "w_10"), OutFormat: transform.FormatJPG, Dst: filepath.Join(t.TempDir(), "o"), Limits: testLimits})
	if !errors.Is(err, ferr.ErrTooLarge) {
		t.Fatalf("err = %v, want too large", err)
	}
}

func TestChildProcessIsKilledOnTimeout(t *testing.T) {
	exe, _ := os.Executable()
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()
	time.Sleep(5 * time.Millisecond)
	_, err := Child{Exe: exe}.Render(ctx, Job{Src: write(t, "x.jpg", encodeJPEG(t, patterned(10, 10), 80)), SrcFormat: sniff.JPEG,
		Spec: spec(t, ""), OutFormat: transform.FormatJPG, Dst: filepath.Join(t.TempDir(), "o"), Limits: testLimits})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline", err)
	}
}

func TestContextEndsTheRenderBetweenSteps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	data := encodeJPEG(t, patterned(50, 50), 80)
	_, err := Vips{}.Render(ctx, Job{Src: write(t, "x.jpg", data), SrcFormat: sniff.JPEG, Spec: spec(t, "w_10"),
		OutFormat: transform.FormatJPG, Dst: filepath.Join(t.TempDir(), "o"), Limits: testLimits})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}
