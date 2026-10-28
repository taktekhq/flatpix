// Package oracle compares Flatpix with Cloudinary on the same transformation.
// Cloudinary is the oracle: for each transformation it fetches the original
// and Cloudinary's rendering from a public cloud at run time, renders the same
// URL with Flatpix, and scores how many pixels agree.
//
// Nothing fetched is stored in the repository.
package oracle

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/davidbyttow/govips/v2/vips"
	"github.com/taktekhq/flatpix/internal/cldurl"
	"github.com/taktekhq/flatpix/internal/render"
	"github.com/taktekhq/flatpix/internal/sniff"
	"github.com/taktekhq/flatpix/internal/transform"
)

// Cases are the transformations compared by default.
var Cases = []string{
	"w_400",
	"h_300",
	"w_300,h_300",
	"w_0.5",
	"dpr_2.0,w_200",
	"c_fit,w_300,h_300",
	"c_limit,w_3000,h_3000",
	"c_mfit,w_3000,h_3000",
	"c_fill,w_300,h_300",
	"c_fill,w_300,h_300,g_north",
	"c_fill,w_300,h_300,g_south_east",
	"c_fill,w_300,h_300,g_auto",
	"c_lfill,w_3000,h_300",
	"c_thumb,w_200,h_200",
	"c_pad,w_400,h_400,b_white",
	"c_pad,w_400,h_400,b_rgb:3355aa",
	"c_lpad,w_3000,h_3000,b_black",
	"c_crop,w_400,h_300",
	"c_crop,w_400,h_300,g_east",
	"ar_16:9,c_fill,w_600",
	"e_grayscale,w_400",
	"a_90,w_300",
	"w_400/a_270",
	"c_fill,w_300,h_300/c_scale,w_150",
	"f_png,w_300",
	"f_webp,w_300",
	"q_30,w_400",
	"q_auto,w_400",
}

// Row is the outcome for one transformation.
type Row struct {
	Transformation string
	CloudinaryW    int
	CloudinaryH    int
	FlatpixW       int
	FlatpixH       int
	Score          float64
	SameSize       bool
	Err            error
}

// Options configure a run.
type Options struct {
	Cloud string // Cloudinary cloud name, "demo" by default
	Image string // public id with extension, "sample.jpg" by default
	Cases []string
	Dir   string // scratch directory for downloads and renders
	Log   io.Writer
}

// Run compares every case.
func Run(ctx context.Context, o Options) ([]Row, error) {
	if o.Cloud == "" {
		o.Cloud = "demo"
	}
	if o.Image == "" {
		o.Image = "sample.jpg"
	}
	if len(o.Cases) == 0 {
		o.Cases = Cases
	}
	client := &http.Client{Timeout: 60 * time.Second}
	base := fmt.Sprintf("https://res.cloudinary.com/%s/image/upload/", o.Cloud)
	orig := filepath.Join(o.Dir, "original")
	if err := download(ctx, client, base+o.Image, orig); err != nil {
		return nil, fmt.Errorf("fetching the original: %w", err)
	}
	var rows []Row
	for i, tr := range o.Cases {
		row := Row{Transformation: tr}
		theirs := filepath.Join(o.Dir, fmt.Sprintf("cld-%d", i))
		if err := download(ctx, client, base+tr+"/"+o.Image, theirs); err != nil {
			row.Err = fmt.Errorf("cloudinary: %w", err)
			rows = append(rows, row)
			continue
		}
		ours := filepath.Join(o.Dir, fmt.Sprintf("fpx-%d", i))
		if err := Flatpix(ctx, orig, o.Image, tr, ours); err != nil {
			row.Err = fmt.Errorf("flatpix: %w", err)
			rows = append(rows, row)
			continue
		}
		s, err := Score(theirs, ours)
		if err != nil {
			row.Err = err
		} else {
			row.Score, row.CloudinaryW, row.CloudinaryH, row.FlatpixW, row.FlatpixH = s.Score, s.AW, s.AH, s.BW, s.BH
			row.SameSize = s.AW == s.BW && s.AH == s.BH
		}
		rows = append(rows, row)
		if o.Log != nil {
			fmt.Fprintf(o.Log, "  %-40s %s\n", tr, summary(row))
		}
	}
	return rows, nil
}

func summary(r Row) string {
	if r.Err != nil {
		return "error: " + r.Err.Error()
	}
	return fmt.Sprintf("%.4f", r.Score)
}

func download(ctx context.Context, c *http.Client, url, dst string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("User-Agent", "flatpix-oracle")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d", url, resp.StatusCode)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, io.LimitReader(resp.Body, 64<<20))
	return err
}

// Flatpix renders the original with a transformation using the same code the
// server uses, and writes the result to dst.
func Flatpix(ctx context.Context, src, publicID, tr, dst string) error {
	req, err := cldurl.Parse("/demo/image/upload/"+tr+"/"+publicID, "")
	if err != nil {
		return err
	}
	spec, err := transform.Compile(req.Chain, false)
	if err != nil {
		return err
	}
	head := make([]byte, sniff.HeadSize)
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	n, _ := f.Read(head)
	f.Close()
	out := transform.FormatFor(spec.Format, req.Ext, "")
	spec.Format = out
	_, err = render.Vips{}.Render(ctx, render.Job{
		Src: src, SrcFormat: sniff.Sniff(head[:n]), Spec: spec, OutFormat: out, Dst: dst,
		Limits: render.Limits{MaxInputPixels: 100_000_000, MaxOutputPixels: 25_000_000},
	})
	return err
}

// Scored is the result of comparing two images.
type Scored struct {
	Score  float64
	AW, AH int
	BW, BH int
}

// Score compares two image files. If their sizes differ, b is resized to the
// size of a first. The score is 1 minus the mean absolute difference over the
// red, green and blue channels, as a fraction of 255: 1.0 is identical.
// Transparency is composited onto white.
func Score(pathA, pathB string) (Scored, error) {
	a, err := loadRGB(pathA)
	if err != nil {
		return Scored{}, err
	}
	defer a.Close()
	b, err := loadRGB(pathB)
	if err != nil {
		return Scored{}, err
	}
	defer b.Close()
	s := Scored{AW: a.Width(), AH: a.Height(), BW: b.Width(), BH: b.Height()}
	if s.AW != s.BW || s.AH != s.BH {
		if err := b.ThumbnailWithSize(s.AW, s.AH, vips.InterestingNone, vips.SizeForce); err != nil {
			return s, err
		}
	}
	if err := b.Subtract(a); err != nil {
		return s, err
	}
	if err := b.Abs(); err != nil {
		return s, err
	}
	mean, err := b.Average()
	if err != nil {
		return s, err
	}
	s.Score = 1 - mean/255
	return s, nil
}

func loadRGB(path string) (*vips.ImageRef, error) {
	img, err := vips.NewImageFromFile(path)
	if err != nil {
		return nil, err
	}
	if err := img.AutoRotate(); err != nil {
		img.Close()
		return nil, err
	}
	if err := img.ToColorSpace(vips.InterpretationSRGB); err != nil {
		img.Close()
		return nil, err
	}
	if img.HasAlpha() {
		if err := img.Flatten(&vips.Color{R: 255, G: 255, B: 255}); err != nil {
			img.Close()
			return nil, err
		}
	}
	return img, nil
}

// Stats summarizes scores.
type Stats struct {
	N       int
	Min     float64
	Median  float64
	Mean    float64
	AtLeast int // rows at or above the target
}

// Summarize computes the minimum, median and mean over the rows that ran.
func Summarize(rows []Row, target float64) Stats {
	var v []float64
	for _, r := range rows {
		if r.Err == nil {
			v = append(v, r.Score)
		}
	}
	st := Stats{N: len(v)}
	if len(v) == 0 {
		return st
	}
	sort.Float64s(v)
	st.Min = v[0]
	if len(v)%2 == 1 {
		st.Median = v[len(v)/2]
	} else {
		st.Median = (v[len(v)/2-1] + v[len(v)/2]) / 2
	}
	var sum float64
	for _, x := range v {
		sum += x
		if x >= target {
			st.AtLeast++
		}
	}
	st.Mean = sum / float64(len(v))
	return st
}

// Table renders the rows as a Markdown table.
func Table(rows []Row, target float64) string {
	var b strings.Builder
	b.WriteString("| transformation | Cloudinary | Flatpix | score | |\n|---|---|---|---|---|\n")
	for _, r := range rows {
		if r.Err != nil {
			fmt.Fprintf(&b, "| `%s` | | | error: %v | |\n", r.Transformation, r.Err)
			continue
		}
		mark := "ok"
		if r.Score < target {
			mark = "below target"
		}
		size := fmt.Sprintf("%dx%d", r.FlatpixW, r.FlatpixH)
		if !r.SameSize {
			size += " (resized)"
		}
		fmt.Fprintf(&b, "| `%s` | %dx%d | %s | %.4f | %s |\n", r.Transformation, r.CloudinaryW, r.CloudinaryH, size, r.Score, mark)
	}
	return b.String()
}
