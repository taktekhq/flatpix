package transform

import (
	"fmt"
	"math"

	"github.com/taktekhq/flatpix/internal/ferr"
)

// StepPlan is one step with every size worked out. The engine runs, in order:
// resize to ResizeW x ResizeH (if set), crop CropW x CropH at CropX,CropY or
// by Interest (if set), pad onto a CanvasW x CanvasH canvas at PadX,PadY
// (if set), grayscale, rotate by Angle.
type StepPlan struct {
	InW, InH         int
	ResizeW, ResizeH int
	CropX, CropY     int
	CropW, CropH     int
	Interest         string // "attention" or "entropy": pick the crop by content
	CanvasW, CanvasH int
	PadX, PadY       int
	Bg               *Color
	Gray             bool
	Angle            int
	OutW, OutH       int // size after this step, rotation included
}

// Plan is a Spec applied to a source of a known size.
type Plan struct {
	Steps      []StepPlan
	OutW, OutH int
	Format     Format
	Quality    Quality
}

// FitArea shrinks w x h to fit an area budget, keeping the aspect ratio. Both
// dimensions come from the area, so a wide image keeps as many pixels as a
// square one. A size already inside the budget is returned unchanged.
func FitArea(w, h int, budget int64) (int, int) {
	if budget < 1 || int64(w)*int64(h) <= budget {
		return w, h
	}
	s := math.Sqrt(float64(budget) / (float64(w) * float64(h)))
	nw, nh := int(math.Floor(float64(w)*s)), int(math.Floor(float64(h)*s))
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	for int64(nw)*int64(nh) > budget && (nw > 1 || nh > 1) {
		if nw >= nh && nw > 1 {
			nw--
		} else if nh > 1 {
			nh--
		}
	}
	return nw, nh
}

// Plan works out the exact operations for a source of srcW x srcH (after
// EXIF orientation). maxOutPixels is the output pixel budget.
func (s *Spec) Plan(srcW, srcH int, maxOutPixels int64) (*Plan, error) {
	p := &Plan{Format: s.Format, Quality: s.Quality}
	w, h := srcW, srcH
	for _, st := range s.Steps {
		sp, err := st.plan(w, h, maxOutPixels)
		if err != nil {
			return nil, err
		}
		p.Steps = append(p.Steps, *sp)
		w, h = sp.OutW, sp.OutH
	}
	if len(p.Steps) == 0 {
		// No pixel operation: still keep the output inside the area budget.
		sp := StepPlan{InW: w, InH: h}
		if int64(w)*int64(h) > maxOutPixels {
			sp.ResizeW, sp.ResizeH = FitArea(w, h, maxOutPixels)
			w, h = sp.ResizeW, sp.ResizeH
		}
		sp.OutW, sp.OutH = w, h
		p.Steps = append(p.Steps, sp)
	}
	p.OutW, p.OutH = w, h
	return p, nil
}

func rdiv(a, b int64) int64 { return (a + b/2) / b }
func cdiv(a, b int64) int64 { return (a + b - 1) / b }

func mini(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func at1(v int64) int {
	if v < 1 {
		return 1
	}
	return int(v)
}

// target resolves w, h and ar against the source size into pixel targets.
// Zero means "not set". explicit tells whether the caller asked for a size.
func (st Step) target(sw, sh int) (tw, th int, explicit bool) {
	dim := func(d Dim, src int) int {
		if !d.Set {
			return 0
		}
		v := d.V
		if d.Rel {
			v = d.V * float64(src)
		}
		if st.DPR > 0 {
			v *= st.DPR
		}
		return at1(int64(math.Round(v)))
	}
	tw, th = dim(st.W, sw), dim(st.H, sh)
	explicit = tw > 0 || th > 0
	if st.AR > 0 && !(tw > 0 && th > 0) {
		switch {
		case tw > 0:
			th = at1(int64(math.Round(float64(tw) / st.AR)))
		case th > 0:
			tw = at1(int64(math.Round(float64(th) * st.AR)))
		default:
			// Only a ratio: the biggest box with that ratio inside the source.
			if float64(sw)/float64(sh) >= st.AR {
				th = sh
				tw = at1(int64(math.Round(float64(sh) * st.AR)))
			} else {
				tw = sw
				th = at1(int64(math.Round(float64(sw) / st.AR)))
			}
			explicit = true
		}
		explicit = true
	}
	return tw, th, explicit
}

// fitSize scales sw x sh to fit inside the given box (a zero side is
// unbounded). limit forbids upscaling, up forbids downscaling.
func fitSize(sw, sh, tw, th int, limit, up bool) (int, int) {
	var w, h int64
	switch {
	case tw > 0 && th > 0:
		if int64(tw)*int64(sh) <= int64(th)*int64(sw) { // width limits
			w, h = int64(tw), rdiv(int64(sh)*int64(tw), int64(sw))
		} else {
			h, w = int64(th), rdiv(int64(sw)*int64(th), int64(sh))
		}
	case tw > 0:
		w, h = int64(tw), rdiv(int64(sh)*int64(tw), int64(sw))
	case th > 0:
		h, w = int64(th), rdiv(int64(sw)*int64(th), int64(sh))
	default:
		return sw, sh
	}
	if (limit && (int(w) > sw || int(h) > sh)) || (up && int(w) < sw && int(h) < sh) {
		return sw, sh
	}
	return at1(w), at1(h)
}

func (st Step) plan(sw, sh int, maxPx int64) (*StepPlan, error) {
	sp := &StepPlan{InW: sw, InH: sh, Bg: st.Bg, Gray: st.Gray, Angle: st.Angle}
	tw, th, explicit := st.target(sw, sh)
	w, h := sw, sh // size before pad and rotation

	setResize := func(rw, rh int) {
		if rw != sw || rh != sh {
			sp.ResizeW, sp.ResizeH = rw, rh
		}
	}
	gx := func(avail int) int { // horizontal offset inside avail spare pixels
		switch st.Gravity {
		case "west", "north_west", "south_west":
			return 0
		case "east", "north_east", "south_east":
			return avail
		}
		return avail / 2
	}
	gy := func(avail int) int {
		switch st.Gravity {
		case "north", "north_west", "north_east":
			return 0
		case "south", "south_west", "south_east":
			return avail
		}
		return avail / 2
	}
	crop := func(cw, ch, rw, rh int) { // crop cw x ch out of an rw x rh image
		sp.CropW, sp.CropH = cw, ch
		sp.CropX, sp.CropY = gx(rw-cw), gy(rh-ch)
		sp.Interest = st.Interest
		w, h = cw, ch
	}
	// cover resizes to the smallest size that covers tw x th.
	cover := func(limitDown bool) (rw, rh int) {
		if int64(tw)*int64(sh) < int64(th)*int64(sw) {
			rh, rw = th, int(cdiv(int64(sw)*int64(th), int64(sh)))
		} else {
			rw, rh = tw, int(cdiv(int64(sh)*int64(tw), int64(sw)))
		}
		if limitDown && (rw >= sw || rh >= sh) {
			return sw, sh
		}
		return rw, rh
	}

	switch st.Crop {
	case "": // scale
		switch {
		case tw > 0 && th > 0:
			setResize(tw, th)
			w, h = tw, th
		case tw > 0:
			h = at1(rdiv(int64(sh)*int64(tw), int64(sw)))
			setResize(tw, h)
			w = tw
		case th > 0:
			w = at1(rdiv(int64(sw)*int64(th), int64(sh)))
			setResize(w, th)
			h = th
		}
	case "fit", "limit", "mfit":
		rw, rh := fitSize(sw, sh, tw, th, st.Crop == "limit", st.Crop == "mfit")
		setResize(rw, rh)
		w, h = rw, rh
	case "fill", "thumb", "lfill":
		if tw == 0 || th == 0 {
			// Only one side: nothing to crop, behaves like scale.
			rw, rh := fitSize(sw, sh, tw, th, st.Crop == "lfill", false)
			setResize(rw, rh)
			w, h = rw, rh
			break
		}
		rw, rh := cover(st.Crop == "lfill")
		if int64(rw)*int64(rh) > 2*maxPx {
			return nil, ferr.New(ferr.ErrBadInput,
				"c_%s to %dx%d needs an intermediate image of %dx%d pixels, over the limit", st.Crop, tw, th, rw, rh)
		}
		setResize(rw, rh)
		crop(mini(tw, rw), mini(th, rh), rw, rh)
	case "pad", "lpad":
		rw, rh := fitSize(sw, sh, tw, th, st.Crop == "lpad", false)
		setResize(rw, rh)
		w, h = rw, rh
		if tw > 0 && th > 0 {
			sp.CanvasW, sp.CanvasH = tw, th
			sp.PadX, sp.PadY = gx(tw-rw), gy(th-rh)
			w, h = tw, th
		}
	case "crop":
		cw, ch := sw, sh
		if tw > 0 {
			cw = mini(tw, sw)
		}
		if th > 0 {
			ch = mini(th, sh)
		}
		if cw != sw || ch != sh {
			crop(cw, ch, sw, sh)
		}
	}

	// A step with no size of its own still stays inside the area budget.
	if !explicit && sp.ResizeW == 0 && sp.CropW == 0 && sp.CanvasW == 0 && int64(w)*int64(h) > maxPx {
		rw, rh := FitArea(w, h, maxPx)
		setResize(rw, rh)
		w, h = rw, rh
	}
	if int64(w)*int64(h) > maxPx {
		return nil, ferr.New(ferr.ErrBadInput,
			"the requested output of %dx%d is %d pixels, over the limit of %d", w, h, int64(w)*int64(h), maxPx)
	}
	if st.Angle == 90 || st.Angle == 270 {
		w, h = h, w
	}
	sp.OutW, sp.OutH = w, h
	return sp, nil
}

// String is a short description for logs.
func (sp StepPlan) String() string {
	return fmt.Sprintf("%dx%d -> resize %dx%d crop %dx%d@%d,%d canvas %dx%d@%d,%d rot %d -> %dx%d",
		sp.InW, sp.InH, sp.ResizeW, sp.ResizeH, sp.CropW, sp.CropH, sp.CropX, sp.CropY,
		sp.CanvasW, sp.CanvasH, sp.PadX, sp.PadY, sp.Angle, sp.OutW, sp.OutH)
}
