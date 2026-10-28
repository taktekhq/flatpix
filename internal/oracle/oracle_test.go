// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Taktek and Nizar Mahmoud

package oracle

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/taktekhq/flatpix/internal/render"
	"github.com/taktekhq/flatpix/internal/testimg"
)

func TestMain(m *testing.M) {
	if err := render.Startup(1); err != nil {
		panic(err)
	}
	code := m.Run()
	render.Shutdown()
	os.Exit(code)
}

func write(t *testing.T, name string, data []byte) string {
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestScore(t *testing.T) {
	a := write(t, "a.png", testimg.PNG(t, testimg.Patterned(100, 80)))
	b := write(t, "b.png", testimg.PNG(t, testimg.Patterned(100, 80)))
	s, err := Score(a, b)
	if err != nil || s.Score != 1 {
		t.Fatalf("identical images: %+v %v", s, err)
	}
	black := write(t, "black.png", testimg.PNG(t, testimg.Solid(50, 50, testimg.Red)))
	white := write(t, "white.png", testimg.PNG(t, testimg.Solid(50, 50, testimg.Blue)))
	s, err = Score(black, white)
	if err != nil {
		t.Fatal(err)
	}
	// red vs blue differ by 255 in two of three channels
	if want := 1 - (255.0*2/3)/255; s.Score < want-0.001 || s.Score > want+0.001 {
		t.Errorf("red vs blue = %v, want %v", s.Score, want)
	}
	// Different sizes are matched before scoring.
	small := write(t, "small.png", testimg.PNG(t, testimg.Solid(10, 10, testimg.Red)))
	big := write(t, "big.png", testimg.PNG(t, testimg.Solid(40, 40, testimg.Red)))
	s, err = Score(big, small)
	if err != nil || s.Score < 0.999 || s.AW != 40 || s.BW != 10 {
		t.Errorf("resized compare: %+v %v", s, err)
	}
}

func TestFlatpixMatchesItself(t *testing.T) {
	src := write(t, "orig.jpg", testimg.JPEG(t, testimg.Patterned(300, 200), 92))
	out := filepath.Join(t.TempDir(), "out.jpg")
	if err := Flatpix(t.Context(), src, "orig.jpg", "c_fill,w_100,h_100", out); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(out); err != nil || st.Size() == 0 {
		t.Fatalf("no output: %v", err)
	}
}

func TestSummarize(t *testing.T) {
	rows := []Row{{Score: 0.99}, {Score: 0.95}, {Score: 1.0}, {Score: 0.5, Err: os.ErrNotExist}}
	st := Summarize(rows, 0.988)
	if st.N != 3 || st.Min != 0.95 || st.Median != 0.99 || st.AtLeast != 2 {
		t.Errorf("%+v", st)
	}
}
