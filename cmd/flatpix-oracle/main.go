// Command flatpix-oracle checks Flatpix against Cloudinary. For each
// transformation it fetches the original and Cloudinary's rendering from a
// public Cloudinary cloud, renders the same URL with Flatpix and prints how
// closely the pixels agree. Nothing it downloads is stored.
//
//	flatpix-oracle                       # the demo cloud, sample.jpg, the default cases
//	flatpix-oracle -image kitten.jpg -only c_fill
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/taktekhq/flatpix/internal/oracle"
	"github.com/taktekhq/flatpix/internal/render"
)

func main() { os.Exit(run()) }

func run() int {
	cloud := flag.String("cloud", "demo", "Cloudinary cloud name to compare against")
	image := flag.String("image", "sample.jpg", "public id of the original, with its extension")
	target := flag.Float64("target", 0.988, "score each transformation should reach")
	only := flag.String("only", "", "run only the transformations that contain this text")
	failBelow := flag.Bool("fail-below", false, "exit 1 if the median is under the target")
	keep := flag.Bool("keep", false, "keep the downloaded and rendered files")
	md := flag.Bool("markdown", false, "print the table as Markdown")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := render.Startup(2); err != nil {
		fmt.Fprintln(os.Stderr, "libvips:", err)
		return 1
	}
	defer render.Shutdown()

	dir, err := os.MkdirTemp("", "flatpix-oracle-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *keep {
		fmt.Fprintln(os.Stderr, "files kept in", dir)
	} else {
		defer os.RemoveAll(dir)
	}

	var cases []string
	for _, c := range oracle.Cases {
		if *only == "" || strings.Contains(c, *only) {
			cases = append(cases, c)
		}
	}
	rows, err := oracle.Run(ctx, oracle.Options{Cloud: *cloud, Image: *image, Cases: cases, Dir: dir})
	if err != nil {
		fmt.Fprintln(os.Stderr, "oracle:", err)
		return 1
	}
	if *md {
		fmt.Print(oracle.Table(rows, *target))
	} else {
		printPlain(rows, *target)
	}
	st := oracle.Summarize(rows, *target)
	fmt.Printf("\n%d compared, min %.4f, median %.4f, mean %.4f, %d at or above %.3f\n", st.N, st.Min, st.Median, st.Mean, st.AtLeast, *target)
	if *failBelow && st.Median < *target {
		return 1
	}
	return 0
}

func printPlain(rows []oracle.Row, target float64) {
	fmt.Printf("%-42s %-12s %-14s %s\n", "transformation", "Cloudinary", "Flatpix", "score")
	for _, r := range rows {
		if r.Err != nil {
			fmt.Printf("%-42s error: %v\n", r.Transformation, r.Err)
			continue
		}
		flag := ""
		if r.Score < target {
			flag = "  <"
		}
		size := fmt.Sprintf("%dx%d", r.FlatpixW, r.FlatpixH)
		if !r.SameSize {
			size += "*"
		}
		fmt.Printf("%-42s %-12s %-14s %.4f%s\n", r.Transformation, fmt.Sprintf("%dx%d", r.CloudinaryW, r.CloudinaryH), size, r.Score, flag)
	}
	fmt.Println("(* = Flatpix size differs and was resized to Cloudinary's before scoring)")
}
