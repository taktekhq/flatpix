// Command flatpix-bench measures a running Flatpix server.
//
// Make a test photo (a synthetic 4000x3000 JPEG with real detail):
//
//	flatpix-bench -gen images/photo.jpg
//
// Then load a server. Each request asks for a different output (the quality
// changes, which does not change the cost of decoding and resizing), so every
// request is a real render. Run the server without a derived cache:
//
//	flatpix-bench -url http://localhost:8080 -n 600 -c 16
//
// With -same every request is identical, which measures the cache hit path.
package main

import (
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func main() { os.Exit(run()) }

func run() int {
	gen := flag.String("gen", "", "write a synthetic test JPEG to this path and exit")
	size := flag.String("size", "4000x3000", "size of the generated JPEG")
	base := flag.String("url", "http://localhost:8080", "server to load")
	path := flag.String("path", "/demo/image/upload/c_fill,w_800,h_600,f_webp,q_{q}/photo.jpg", "request path; {q} becomes a number from 1 to 99")
	n := flag.Int("n", 400, "requests to send")
	c := flag.Int("c", 8, "concurrent clients")
	warm := flag.Int("warmup", 8, "requests to send first and ignore")
	same := flag.Bool("same", false, "send the same URL every time (measures the cache hit path)")
	flag.Parse()

	if *gen != "" {
		var w, h int
		if _, err := fmt.Sscanf(*size, "%dx%d", &w, &h); err != nil {
			fmt.Fprintln(os.Stderr, "bad -size:", err)
			return 2
		}
		if err := generate(*gen, w, h); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Printf("wrote %s (%dx%d)\n", *gen, w, h)
		return 0
	}

	url := func(i int) string {
		q := 1 + i%99
		if *same {
			q = 80
		}
		return *base + strings.ReplaceAll(*path, "{q}", fmt.Sprint(q))
	}
	client := &http.Client{Timeout: 2 * time.Minute, Transport: &http.Transport{MaxIdleConnsPerHost: *c + 4}}
	get := func(i int) (time.Duration, int, int64) {
		start := time.Now()
		resp, err := client.Get(url(i))
		if err != nil {
			return time.Since(start), 0, 0
		}
		nb, _ := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return time.Since(start), resp.StatusCode, nb
	}
	for i := 0; i < *warm; i++ {
		get(1000 + i)
	}

	var next atomic.Int64
	var mu sync.Mutex
	var lat []time.Duration
	codes := map[int]int{}
	var bytes int64
	var wg sync.WaitGroup
	t0 := time.Now()
	for w := 0; w < *c; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= *n {
					return
				}
				d, code, nb := get(i)
				mu.Lock()
				lat = append(lat, d)
				codes[code]++
				bytes += nb
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	total := time.Since(t0)

	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pct := func(p float64) time.Duration {
		if len(lat) == 0 {
			return 0
		}
		return lat[min(len(lat)-1, int(math.Ceil(p*float64(len(lat))))-1)]
	}
	fmt.Printf("requests     %d (%d clients)\n", len(lat), *c)
	fmt.Printf("status       %v\n", codes)
	fmt.Printf("duration     %.2fs\n", total.Seconds())
	fmt.Printf("throughput   %.1f requests/s\n", float64(len(lat))/total.Seconds())
	fmt.Printf("latency      p50 %v   p95 %v   p99 %v   max %v\n", ms(pct(.50)), ms(pct(.95)), ms(pct(.99)), ms(lat[len(lat)-1]))
	fmt.Printf("avg response %d bytes\n", bytes/int64(max(len(lat), 1)))
	if codes[200] != len(lat) {
		return 1
	}
	return 0
}

func ms(d time.Duration) string { return fmt.Sprintf("%.0f ms", float64(d.Microseconds())/1000) }

// generate writes a photo-like JPEG: smooth colour fields plus fine noise, so
// the file is a few MB and decoding it costs what a real photo costs.
func generate(path string, w, h int) error {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	seed := uint32(12345)
	rnd := func() uint32 { seed = seed*1664525 + 1013904223; return seed >> 24 }
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			fx, fy := float64(x)/float64(w), float64(y)/float64(h)
			r := 128 + 100*math.Sin(fx*12+fy*3)
			g := 128 + 100*math.Sin(fy*9-fx*5)
			b := 128 + 100*math.Cos((fx+fy)*14)
			n := float64(rnd()%32) - 16
			img.SetRGBA(x, y, color.RGBA{clamp(r + n), clamp(g + n), clamp(b + n), 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return jpeg.Encode(f, img, &jpeg.Options{Quality: 90})
}

func clamp(v float64) uint8 { return uint8(math.Max(0, math.Min(255, v))) }
