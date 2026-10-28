// Package transform turns the parameters of a Cloudinary-style URL into a
// Spec, a normal form for cache keys, and (given the source size) a Plan of
// exact pixel operations. Nothing here touches libvips, so it is testable
// without it.
package transform

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/taktekhq/flatpix/internal/cldurl"
	"github.com/taktekhq/flatpix/internal/ferr"
)

// Format is an output format.
type Format string

const (
	// FormatOriginal keeps the source format.
	FormatOriginal Format = ""
	// FormatAuto negotiates avif, then webp, then the original, from Accept.
	FormatAuto Format = "auto"
	FormatJPG  Format = "jpg"
	FormatPNG  Format = "png"
	FormatWEBP Format = "webp"
	FormatAVIF Format = "avif"
	FormatGIF  Format = "gif"
)

// Dim is a width or a height: pixels, or a fraction of the source size.
type Dim struct {
	Set bool
	Rel bool    // V is a fraction of the source size
	V   float64 // pixels (already rounded) or fraction
}

// Color is an RGBA color.
type Color struct{ R, G, B, A uint8 }

// Step is one "/" separated component of the chain.
type Step struct {
	W, H     Dim
	Crop     string  // "" means scale
	Gravity  string  // "" means center; compass name or "auto"
	Interest string  // "attention" or "entropy" when Gravity is auto
	AR       float64 // 0 when not set
	DPR      float64 // 0 when not set
	Bg       *Color
	Gray     bool
	Angle    int // 0, 90, 180, 270
}

// Quality is q_. Num wins over Auto.
type Quality struct {
	Num  int    // 1..100, or 0 when not set
	Auto string // "", "best", "good", "eco", "low"
}

// Spec is a compiled transformation chain.
type Spec struct {
	Steps   []Step
	Format  Format
	Quality Quality
	// Ignored lists the parameters skipped in lenient mode.
	Ignored []string
}

var (
	numRe    = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
	hexRe    = regexp.MustCompile(`^[0-9a-fA-F]+$`)
	arPairRe = regexp.MustCompile(`^([0-9]+(\.[0-9]+)?):([0-9]+(\.[0-9]+)?)$`)
)

const maxDim = 65535

var cropModes = map[string]bool{
	"scale": true, "fit": true, "limit": true, "mfit": true, "fill": true,
	"lfill": true, "pad": true, "lpad": true, "crop": true, "thumb": true,
}

var compass = map[string]bool{
	"center": true, "north": true, "south": true, "east": true, "west": true,
	"north_east": true, "north_west": true, "south_east": true, "south_west": true,
}

var namedColors = map[string]Color{
	"white": {255, 255, 255, 255}, "black": {0, 0, 0, 255}, "red": {255, 0, 0, 255},
	"green": {0, 128, 0, 255}, "blue": {0, 0, 255, 255}, "yellow": {255, 255, 0, 255},
	"cyan": {0, 255, 255, 255}, "aqua": {0, 255, 255, 255}, "magenta": {255, 0, 255, 255},
	"fuchsia": {255, 0, 255, 255}, "gray": {128, 128, 128, 255}, "grey": {128, 128, 128, 255},
	"silver": {192, 192, 192, 255}, "orange": {255, 165, 0, 255}, "purple": {128, 0, 128, 255},
	"pink": {255, 192, 203, 255}, "brown": {165, 42, 42, 255}, "navy": {0, 0, 128, 255},
	"teal": {0, 128, 128, 255}, "lime": {0, 255, 0, 255}, "maroon": {128, 0, 0, 255},
	"olive": {128, 128, 0, 255}, "transparent": {0, 0, 0, 0},
}

// Compile turns the parsed chain into a Spec. In strict mode a parameter that
// cannot be honored is a bad input error that names it. In lenient mode it is
// skipped and listed in Spec.Ignored.
func Compile(chain [][]cldurl.Param, lenient bool) (*Spec, error) {
	s := &Spec{}
	for _, comp := range chain {
		var st Step
		any := false
		for _, p := range comp {
			ok, err := apply(s, &st, p)
			if err != nil {
				if lenient {
					s.Ignored = append(s.Ignored, p.String())
					continue
				}
				return nil, err
			}
			if ok {
				any = true
			}
		}
		if any {
			s.Steps = append(s.Steps, st)
		}
	}
	return s, nil
}

func unsupported(p cldurl.Param, why string) error {
	return ferr.New(ferr.ErrBadInput, "unsupported transformation parameter %q: %s", p.String(), why)
}

// apply sets one parameter. It reports whether the parameter belongs to the
// step (f_ and q_ belong to the whole chain).
func apply(s *Spec, st *Step, p cldurl.Param) (bool, error) {
	switch p.Key {
	case "w", "h":
		d, err := parseDim(p)
		if err != nil {
			return false, err
		}
		if p.Key == "w" {
			st.W = d
		} else {
			st.H = d
		}
	case "c":
		if !cropModes[p.Value] {
			return false, unsupported(p, "crop modes are scale, fit, limit, mfit, fill, lfill, pad, lpad, crop, thumb")
		}
		st.Crop = p.Value
		if p.Value == "scale" {
			st.Crop = ""
		}
	case "g":
		switch {
		case compass[p.Value]:
			st.Gravity = p.Value
			if p.Value == "center" {
				st.Gravity = ""
			}
		case p.Value == "auto" || p.Value == "auto:attention":
			st.Gravity, st.Interest = "auto", "attention"
		case p.Value == "auto:entropy":
			st.Gravity, st.Interest = "auto", "entropy"
		default:
			return false, unsupported(p, "gravity is center, the eight compass directions (north, north_east, ...), auto, auto:entropy")
		}
	case "ar":
		ar, err := parseAR(p)
		if err != nil {
			return false, err
		}
		st.AR = ar
	case "dpr":
		if !numRe.MatchString(p.Value) {
			return false, unsupported(p, "dpr must be a number such as 2.0")
		}
		v, _ := strconv.ParseFloat(p.Value, 64)
		if v <= 0 || v > 10 {
			return false, unsupported(p, "dpr must be above 0 and at most 10")
		}
		st.DPR = v
	case "b":
		c, err := parseColor(p)
		if err != nil {
			return false, err
		}
		st.Bg = &c
	case "e":
		if p.Value != "grayscale" {
			return false, unsupported(p, "the only supported effect is e_grayscale")
		}
		st.Gray = true
	case "a":
		switch p.Value {
		case "90", "-270":
			st.Angle = 90
		case "180", "-180":
			st.Angle = 180
		case "270", "-90":
			st.Angle = 270
		case "auto", "exif":
			// EXIF orientation is always applied, so this changes nothing.
		default:
			return false, unsupported(p, "angle is 90, 180, 270, auto or exif")
		}
	case "f":
		switch p.Value {
		case "auto":
			s.Format = FormatAuto
		case "jpg", "jpeg":
			s.Format = FormatJPG
		case "png", "webp", "avif", "gif":
			s.Format = Format(p.Value)
		default:
			return false, unsupported(p, "format is auto, jpg, png, webp, avif or gif")
		}
		return false, nil
	case "q":
		q, err := parseQuality(p)
		if err != nil {
			return false, err
		}
		s.Quality = q
		return false, nil
	default:
		hint := ""
		if len(p.Key) <= 3 {
			hint = "; if this is a folder name, put a version segment such as v1/ before the folder"
		}
		return false, unsupported(p, "unknown parameter"+hint)
	}
	return true, nil
}

func parseDim(p cldurl.Param) (Dim, error) {
	if !numRe.MatchString(p.Value) {
		return Dim{}, unsupported(p, "expected a number such as 300 or 0.5")
	}
	v, _ := strconv.ParseFloat(p.Value, 64)
	if v <= 0 {
		return Dim{}, unsupported(p, "must be above 0")
	}
	if strings.Contains(p.Value, ".") && v <= 1 {
		return Dim{Set: true, Rel: true, V: v}, nil
	}
	v = math.Round(v)
	if v < 1 || v > maxDim {
		return Dim{}, unsupported(p, fmt.Sprintf("must be between 1 and %d pixels", maxDim))
	}
	return Dim{Set: true, V: v}, nil
}

func parseAR(p cldurl.Param) (float64, error) {
	var v float64
	if m := arPairRe.FindStringSubmatch(p.Value); m != nil {
		a, _ := strconv.ParseFloat(m[1], 64)
		b, _ := strconv.ParseFloat(m[3], 64)
		if a <= 0 || b <= 0 {
			return 0, unsupported(p, "both sides of the ratio must be above 0")
		}
		v = a / b
	} else if numRe.MatchString(p.Value) {
		v, _ = strconv.ParseFloat(p.Value, 64)
	} else {
		return 0, unsupported(p, "expected a ratio such as 16:9 or a decimal such as 1.5")
	}
	if v <= 0 || v > 1000 || v < 0.001 {
		return 0, unsupported(p, "ratio out of range")
	}
	return v, nil
}

func parseColor(p cldurl.Param) (Color, error) {
	if c, ok := namedColors[strings.ToLower(p.Value)]; ok {
		return c, nil
	}
	if h, ok := strings.CutPrefix(strings.ToLower(p.Value), "rgb:"); ok && hexRe.MatchString(h) {
		switch len(h) {
		case 3, 4:
			var ex strings.Builder
			for _, r := range h {
				ex.WriteRune(r)
				ex.WriteRune(r)
			}
			h = ex.String()
		case 6, 8:
		default:
			return Color{}, unsupported(p, "rgb: needs 3, 4, 6 or 8 hex digits")
		}
		if len(h) == 6 {
			h += "ff"
		}
		v, _ := strconv.ParseUint(h, 16, 32)
		return Color{uint8(v >> 24), uint8(v >> 16), uint8(v >> 8), uint8(v)}, nil
	}
	return Color{}, unsupported(p, "background is a color name (white, black, red, ...) or rgb:RRGGBB")
}

func parseQuality(p cldurl.Param) (Quality, error) {
	switch p.Value {
	case "auto", "auto:good":
		return Quality{Auto: "good"}, nil
	case "auto:best", "auto:eco", "auto:low":
		return Quality{Auto: strings.TrimPrefix(p.Value, "auto:")}, nil
	}
	if n, err := strconv.Atoi(p.Value); err == nil && n >= 1 && n <= 100 && !strings.HasPrefix(p.Value, "+") {
		return Quality{Num: n}, nil
	}
	return Quality{}, unsupported(p, "quality is a number from 1 to 100, auto, auto:best, auto:good, auto:eco or auto:low")
}

// Normalize returns the canonical text of the transformation. Spellings that
// produce the same output give the same text. It is part of the cache key.
func (s *Spec) Normalize() string {
	var parts []string
	for _, st := range s.Steps {
		if t := st.normalize(); t != "" {
			parts = append(parts, t)
		}
	}
	var tail []string
	if s.Format != FormatOriginal {
		tail = append(tail, "f_"+string(s.Format))
	}
	if s.Quality.Num > 0 {
		tail = append(tail, "q_"+strconv.Itoa(s.Quality.Num))
	} else if s.Quality.Auto != "" {
		tail = append(tail, "q_auto:"+s.Quality.Auto)
	}
	if len(tail) > 0 {
		parts = append(parts, strings.Join(tail, ","))
	}
	return strings.Join(parts, "/")
}

func (st Step) normalize() string {
	var kv []string
	add := func(k, v string) { kv = append(kv, k+"_"+v) }
	if st.Angle != 0 {
		add("a", strconv.Itoa(st.Angle))
	}
	if st.AR > 0 {
		add("ar", strconv.FormatFloat(st.AR, 'f', 6, 64))
	}
	if st.Bg != nil {
		add("b", fmt.Sprintf("rgb:%02x%02x%02x%02x", st.Bg.R, st.Bg.G, st.Bg.B, st.Bg.A))
	}
	if st.Crop != "" {
		add("c", st.Crop)
	}
	if st.DPR > 0 && st.DPR != 1 {
		add("dpr", strconv.FormatFloat(st.DPR, 'f', -1, 64))
	}
	if st.Gray {
		add("e", "grayscale")
	}
	if st.Gravity != "" {
		g := st.Gravity
		if g == "auto" {
			g = "auto:" + st.Interest
		}
		add("g", g)
	}
	if st.H.Set {
		add("h", st.H.norm())
	}
	if st.W.Set {
		add("w", st.W.norm())
	}
	sort.Strings(kv)
	return strings.Join(kv, ",")
}

func (d Dim) norm() string {
	if d.Rel {
		s := strconv.FormatFloat(d.V, 'f', -1, 64)
		if !strings.Contains(s, ".") {
			s += ".0"
		}
		return s
	}
	return strconv.Itoa(int(d.V))
}

// FormatFor is the output format for a request: f_ wins, then the URL
// extension (jpg, png, webp, avif, gif), then the source format (""). f_auto
// is settled from the Accept header.
func FormatFor(f Format, urlExt, accept string) Format {
	if f == FormatOriginal {
		switch urlExt {
		case "jpg", "jpeg":
			f = FormatJPG
		case "png", "webp", "avif", "gif":
			f = Format(urlExt)
		}
	}
	return ResolveFormat(f, accept)
}

// ResolveFormat turns f_auto into a concrete format using the Accept header:
// avif, then webp, then the original (""). Wildcards never count as support
// for a modern format. Other formats are returned unchanged.
func ResolveFormat(f Format, accept string) Format {
	if f != FormatAuto {
		return f
	}
	ok := map[string]bool{}
	for _, item := range strings.Split(accept, ",") {
		mt, params, _ := strings.Cut(strings.TrimSpace(item), ";")
		mt = strings.ToLower(strings.TrimSpace(mt))
		q := 1.0
		for _, param := range strings.Split(params, ";") {
			k, v, _ := strings.Cut(strings.TrimSpace(param), "=")
			if strings.EqualFold(strings.TrimSpace(k), "q") {
				if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
					q = f
				}
			}
		}
		if q > 0 {
			ok[mt] = true
		}
	}
	switch {
	case ok["image/avif"]:
		return FormatAVIF
	case ok["image/webp"]:
		return FormatWEBP
	}
	return FormatOriginal
}

// JPEG is the JPEG quality for q. The defaults and the auto tiers are
// approximations of what a viewer would call best, good, eco and low.
func (q Quality) JPEG() int {
	if q.Num > 0 {
		return q.Num
	}
	switch q.Auto {
	case "best":
		return 90
	case "good", "auto":
		return 80
	case "eco":
		return 68
	case "low":
		return 52
	}
	return 90
}

// WebP is the WebP quality for q.
func (q Quality) WebP() int {
	if q.Num > 0 || q.Auto != "" {
		return q.JPEG()
	}
	return 85
}

// AVIF is the AVIF quality for q. AVIF reaches the same look at a lower number.
func (q Quality) AVIF() int {
	if q.Num == 0 && q.Auto == "" {
		return 60
	}
	n := q.JPEG() - 25
	if n < 1 {
		n = 1
	}
	return n
}
