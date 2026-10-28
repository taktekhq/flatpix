package render

import (
	"bytes"
	"context"
	"math"
	"os"

	"github.com/davidbyttow/govips/v2/vips"
	"github.com/taktekhq/flatpix/internal/ferr"
	"github.com/taktekhq/flatpix/internal/sniff"
	"github.com/taktekhq/flatpix/internal/transform"
)

// Startup starts libvips. concurrency is the number of threads one operation
// may use; with several seats this is usually 1. The operation cache is off:
// a server does not repeat the same operation on the same image.
func Startup(concurrency int) error {
	vips.LoggingSettings(nil, vips.LogLevelWarning)
	if err := vips.Startup(&vips.Config{ConcurrencyLevel: concurrency}); err != nil {
		return err
	}
	tiffUnlimited = probeTIFFUnlimited()
	return nil
}

// TIFFUnlimited reports whether the TIFF loader's unlimited flag is in use.
func TIFFUnlimited() bool { return tiffUnlimited }

// Shutdown stops libvips.
func Shutdown() { vips.Shutdown() }

// Vips renders web images in process with libvips.
type Vips struct{}

type header struct {
	w, h        int // after EXIF orientation
	orientation int
	hasAlpha    bool
}

// tiffUnlimited is set at startup when this libvips' TIFF loader has the
// "unlimited" flag (it removes libtiff's memory caps, so large Photoshop tags
// do not fail the load). Older libvips, such as Debian 13's 8.16, lack it.
var tiffUnlimited bool

// thumbParams are the loader options for thumbnail-from-file. TIFF gets the
// loader's unlimited flag when this libvips has one.
func thumbParams(f sniff.Format) *vips.ImportParams {
	if f != sniff.TIFF || !tiffUnlimited {
		return nil
	}
	p := &vips.ImportParams{}
	p.SvgUnlimited.Set(true) // govips names it for SVG; in the option string it is plain "unlimited"
	return p
}

// probeTIFFUnlimited tries the flag on a tiny TIFF.
func probeTIFFUnlimited() bool {
	px, err := vips.Black(2, 2)
	if err != nil {
		return false
	}
	defer px.Close()
	buf, _, err := px.ExportTiff(vips.NewTiffExportParams())
	if err != nil {
		return false
	}
	p := &vips.ImportParams{}
	p.SvgUnlimited.Set(true)
	img, err := vips.LoadThumbnailFromBuffer(buf, 2, 2, vips.InterestingNone, vips.SizeForce, p)
	if err != nil {
		return false
	}
	img.Close()
	return true
}

// inspect reads the header only. The reader is sequential and lazy, so no
// pixel is decoded and the file is never held in memory.
func inspect(path string, f sniff.Format, lim Limits) (header, error) {
	file, err := os.Open(path)
	if err != nil {
		return header{}, ferr.Wrap(ferr.ErrInternal, err, "could not open the original")
	}
	defer file.Close()
	p := &vips.ImportParams{}
	p.Access.Set(vips.AccessSequential)
	ref, err := vips.LoadImageFromReader(file, p)
	if err != nil {
		return header{}, ferr.Wrap(ferr.ErrBadInput, err, "the original could not be read as a %s image", f)
	}
	defer ref.Close()
	h := header{w: ref.Width(), h: ref.PageHeight(), orientation: ref.Orientation(), hasAlpha: ref.HasAlpha()}
	if h.h <= 0 {
		h.h = ref.Height()
	}
	if h.w < 1 || h.h < 1 {
		return header{}, ferr.New(ferr.ErrBadInput, "the original has no pixels")
	}
	if lim.MaxInputPixels > 0 && int64(h.w)*int64(h.h) > lim.MaxInputPixels {
		return header{}, ferr.New(ferr.ErrTooLarge, "the original is %dx%d pixels, the limit is %d pixels", h.w, h.h, lim.MaxInputPixels)
	}
	if h.orientation >= 5 && h.orientation <= 8 {
		h.w, h.h = h.h, h.w
	}
	return h, nil
}

func alphaFormat(f transform.Format) bool {
	return f == transform.FormatPNG || f == transform.FormatWEBP || f == transform.FormatAVIF || f == transform.FormatGIF
}

// originalFormat maps the source format to the output format that keeps it.
// Formats a browser cannot show (TIFF, HEIC) become JPEG, or PNG if they
// have transparency.
func originalFormat(f sniff.Format, hasAlpha bool) transform.Format {
	switch f {
	case sniff.JPEG:
		return transform.FormatJPG
	case sniff.PNG:
		return transform.FormatPNG
	case sniff.WEBP:
		return transform.FormatWEBP
	case sniff.AVIF:
		return transform.FormatAVIF
	case sniff.GIF:
		return transform.FormatGIF
	}
	if hasAlpha {
		return transform.FormatPNG
	}
	return transform.FormatJPG
}

func interest(s string) vips.Interesting {
	if s == "entropy" {
		return vips.InterestingEntropy
	}
	return vips.InterestingAttention
}

// Render implements Renderer.
func (Vips) Render(ctx context.Context, j Job) (*Result, error) {
	hdr, err := inspect(j.Src, j.SrcFormat, j.Limits)
	if err != nil {
		return nil, err
	}
	plan, err := j.Spec.Plan(hdr.w, hdr.h, j.Limits.MaxOutputPixels)
	if err != nil {
		return nil, err
	}
	outFmt := j.OutFormat
	if outFmt == transform.FormatOriginal {
		outFmt = originalFormat(j.SrcFormat, hdr.hasAlpha)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	img, err := load(j, hdr, plan)
	if err != nil {
		return nil, err
	}
	defer func() { img.Close() }() // img may be replaced by a step

	for i, sp := range plan.Steps {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		next, err := applyStep(img, sp, outFmt, j.Limits.MaxOutputPixels)
		img = next
		if err != nil {
			if ferr.Kind(err) != ferr.ErrInternal {
				return nil, err
			}
			return nil, ferr.Wrap(ferr.ErrInternal, err, "render step %d failed", i+1)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := finish(img, outFmt, plan); err != nil {
		return nil, ferr.Wrap(ferr.ErrInternal, err, "colour conversion failed")
	}
	return encode(img, outFmt, plan, j.Dst)
}

// shrinkAbove is the downscale ratio at which WebP and TIFF are shrunk by the
// loader before the final resize.
const shrinkAbove = 3.0

// jpegShrink is the biggest JPEG DCT scale (1, 2, 4 or 8) that still leaves
// the image at least as big as the target on both axes.
func jpegShrink(ratio float64) int {
	switch {
	case ratio >= 8:
		return 8
	case ratio >= 4:
		return 4
	case ratio >= 2:
		return 2
	}
	return 1
}

// load opens the source with libvips thumbnail-from-file. The loader shrinks
// while it decodes, so a huge original is never decoded at full size and never
// held in memory:
//
//   - JPEG: DCT scaling, by the biggest factor that keeps the image at least the
//     size of the target. The final resize then runs from that image. A
//     4000x3000 photo going to 800 pixels is decoded at 1000x750.
//   - WebP and TIFF: for a downscale of 3x or more, to twice the final size.
//   - Other formats cannot shrink while decoding and are read at their own size.
//
// EXIF orientation is applied here.
func load(j Job, hdr header, plan *transform.Plan) (*vips.ImageRef, error) {
	first := plan.Steps[0]
	w, h := hdr.w, hdr.h
	if first.ResizeW > 0 {
		rw, rh := hdr.w, hdr.h // the source as the resize will see it
		if first.Angle == 90 || first.Angle == 270 {
			rw, rh = rh, rw
		}
		// The smaller axis ratio keeps both axes at least target size.
		ratio := math.Min(float64(rw)/float64(first.ResizeW), float64(rh)/float64(first.ResizeH))
		switch j.SrcFormat {
		case sniff.JPEG:
			if f := jpegShrink(ratio); f > 1 {
				w, h = max(1, hdr.w/f), max(1, hdr.h/f)
			}
		case sniff.WEBP, sniff.TIFF:
			if ratio >= shrinkAbove {
				k := 2 / ratio
				w, h = max(1, int(math.Round(float64(hdr.w)*k))), max(1, int(math.Round(float64(hdr.h)*k)))
			}
		}
	}
	img, err := vips.LoadThumbnailFromFile(j.Src, w, h, vips.InterestingNone, vips.SizeForce, thumbParams(j.SrcFormat))
	if err != nil {
		return nil, ferr.Wrap(ferr.ErrBadInput, err, "the original could not be decoded")
	}
	return img, nil
}

// resizeTo scales to exactly tw x th. Mitchell is softer than libvips'
// default Lanczos3 and agrees better with Cloudinary's output (see the oracle).
func resizeTo(img *vips.ImageRef, tw, th int) error {
	w, h := img.Width(), img.Height()
	if w == tw && h == th {
		return nil
	}
	if err := img.ResizeWithVScale(float64(tw)/float64(w), float64(th)/float64(h), vips.KernelMitchell); err != nil {
		return err
	}
	if img.Width() != tw || img.Height() != th { // rounding guard
		return img.ThumbnailWithSize(tw, th, vips.InterestingNone, vips.SizeForce)
	}
	return nil
}

// materialize copies an image into memory. Content-aware cropping reads the
// image twice, which a streamed image does not allow.
func materialize(img *vips.ImageRef, maxPixels int64) (*vips.ImageRef, error) {
	if int64(img.Width())*int64(img.Height()) > maxPixels {
		return nil, ferr.New(ferr.ErrBadInput, "g_auto needs the whole image in memory, which is limited to %d pixels", maxPixels)
	}
	buf, err := img.ToBytes()
	if err != nil {
		return nil, err
	}
	out, err := vips.NewImageFromMemory(buf, img.Width(), img.Height(), img.Bands(), img.BandFormat(), img.Interpretation())
	if err != nil {
		return nil, err
	}
	img.Close()
	return out, nil
}

// applyStep runs one step: rotate, resize, crop, pad, grayscale. Rotation
// comes first, as in Cloudinary, so a_90,w_300 gives a 300 pixel wide result.
// The image may be replaced (a content-aware crop copies it into memory).
func applyStep(img *vips.ImageRef, sp transform.StepPlan, outFmt transform.Format, maxPx int64) (*vips.ImageRef, error) {
	var err error
	switch sp.Angle {
	case 90:
		err = img.Rotate(vips.Angle90)
	case 180:
		err = img.Rotate(vips.Angle180)
	case 270:
		err = img.Rotate(vips.Angle270)
	}
	if err != nil {
		return img, err
	}
	if sp.ResizeW > 0 {
		if err := resizeTo(img, sp.ResizeW, sp.ResizeH); err != nil {
			return img, err
		}
	}
	if sp.CropW > 0 {
		cw, ch := min(sp.CropW, img.Width()), min(sp.CropH, img.Height())
		if sp.Interest != "" {
			m, err := materialize(img, maxPx)
			if err != nil {
				return img, err
			}
			img = m
			if err := img.SmartCrop(cw, ch, interest(sp.Interest)); err != nil {
				return img, err
			}
		} else {
			x := min(sp.CropX, img.Width()-cw)
			y := min(sp.CropY, img.Height()-ch)
			if err := img.ExtractArea(x, y, cw, ch); err != nil {
				return img, err
			}
		}
	}
	if sp.CanvasW > 0 {
		bg := transform.Color{R: 255, G: 255, B: 255, A: 255}
		if sp.Bg != nil {
			bg = *sp.Bg
		} else if alphaFormat(outFmt) {
			bg = transform.Color{}
		}
		if bg.A < 255 && !img.HasAlpha() {
			if err := img.AddAlpha(); err != nil {
				return img, err
			}
		}
		c := &vips.ColorRGBA{R: bg.R, G: bg.G, B: bg.B, A: bg.A}
		if err := img.EmbedBackgroundRGBA(sp.PadX, sp.PadY, sp.CanvasW, sp.CanvasH, c); err != nil {
			return img, err
		}
	}
	if sp.Gray {
		err = grayscale(img)
	}
	return img, err
}

// grayscale weights the gamma-encoded channels with the Rec. 709 luma
// coefficients. libvips' own black-and-white conversion works in linear light
// and comes out noticeably brighter than Cloudinary's.
func grayscale(img *vips.ImageRef) error {
	n := img.Bands()
	if n < 3 {
		return nil // already grey
	}
	const r, g, b = 0.2126, 0.7152, 0.0722
	m := make([][]float64, n)
	for i := range m {
		m[i] = make([]float64, n)
		if i < 3 {
			m[i][0], m[i][1], m[i][2] = r, g, b
		} else {
			m[i][i] = 1 // alpha passes through
		}
	}
	return img.Recomb(m)
}

// finish brings the image to sRGB and flattens transparency for formats that
// have none.
func finish(img *vips.ImageRef, outFmt transform.Format, plan *transform.Plan) error {
	if img.HasICCProfile() && !bytes.Contains(img.GetICCProfile(), []byte("sRGB")) &&
		img.Interpretation() != vips.InterpretationBW {
		// A wide-gamut profile (Display P3, Adobe RGB) would shift colour once
		// the profile is stripped, so convert to sRGB first.
		if err := img.TransformICCProfile(vips.SRGBIEC6196621ICCProfilePath); err != nil {
			return err
		}
	}
	if it := img.Interpretation(); it != vips.InterpretationSRGB && it != vips.InterpretationBW {
		if err := img.ToColorSpace(vips.InterpretationSRGB); err != nil {
			return err
		}
	}
	if !alphaFormat(outFmt) && img.HasAlpha() {
		bg := &vips.Color{R: 255, G: 255, B: 255}
		for i := len(plan.Steps) - 1; i >= 0; i-- {
			if b := plan.Steps[i].Bg; b != nil {
				bg = &vips.Color{R: b.R, G: b.G, B: b.B}
				break
			}
		}
		return img.Flatten(bg)
	}
	return nil
}

func encode(img *vips.ImageRef, f transform.Format, plan *transform.Plan, dst string) (*Result, error) {
	var (
		data []byte
		err  error
	)
	q := plan.Quality
	switch f {
	case transform.FormatJPG:
		p := vips.NewJpegExportParams()
		p.Quality, p.StripMetadata, p.Interlace, p.OptimizeCoding = q.JPEG(), true, false, true
		data, _, err = img.ExportJpeg(p)
	case transform.FormatPNG:
		p := vips.NewPngExportParams()
		p.StripMetadata, p.Interlace = true, false
		data, _, err = img.ExportPng(p)
	case transform.FormatWEBP:
		p := vips.NewWebpExportParams()
		p.Quality, p.StripMetadata = q.WebP(), true
		data, _, err = img.ExportWebp(p)
	case transform.FormatAVIF:
		p := vips.NewAvifExportParams()
		p.Quality, p.StripMetadata, p.Effort = q.AVIF(), true, 4
		data, _, err = img.ExportAvif(p)
	case transform.FormatGIF:
		p := vips.NewGifExportParams()
		p.StripMetadata = true
		data, _, err = img.ExportGIF(p)
	default:
		return nil, ferr.New(ferr.ErrInternal, "no encoder for %q", f)
	}
	if err != nil {
		if f == transform.FormatAVIF || f == transform.FormatGIF {
			return nil, ferr.Wrap(ferr.ErrBadInput, err, "%s output is not available in this build", f)
		}
		return nil, ferr.Wrap(ferr.ErrInternal, err, "encoding %s failed", f)
	}
	if len(data) == 0 {
		return nil, ferr.New(ferr.ErrInternal, "the encoder produced no output")
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return nil, ferr.Wrap(ferr.ErrInternal, err, "could not write the output")
	}
	ct := map[transform.Format]string{
		transform.FormatJPG: "image/jpeg", transform.FormatPNG: "image/png", transform.FormatWEBP: "image/webp",
		transform.FormatAVIF: "image/avif", transform.FormatGIF: "image/gif",
	}[f]
	return &Result{Size: int64(len(data)), ContentType: ct, Format: string(f), Width: img.Width(), Height: img.Height()}, nil
}
