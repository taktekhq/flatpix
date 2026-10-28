// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Taktek and Nizar Mahmoud

// Command flatpix is an image server that reads Cloudinary-style URLs.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/davidbyttow/govips/v2/vips"
	"github.com/taktekhq/flatpix/internal/config"
	"github.com/taktekhq/flatpix/internal/fetch"
	"github.com/taktekhq/flatpix/internal/origin"
	"github.com/taktekhq/flatpix/internal/render"
	"github.com/taktekhq/flatpix/internal/scratch"
	"github.com/taktekhq/flatpix/internal/server"
)

var version = "dev"

func main() {
	// A render child is this same binary started with FLATPIX_CHILD=render.
	if os.Getenv("FLATPIX_CHILD") == "render" {
		os.Exit(render.RunChild(os.Stdin, os.Stdout))
	}
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) > 0 && args[0] == "healthcheck" {
		return healthcheck(os.Getenv("FLATPIX_LISTEN"))
	}
	cfg, err := config.Load(args, os.Getenv)
	var help *config.HelpError
	if errors.As(err, &help) {
		fmt.Fprint(os.Stderr, help.Error())
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "flatpix:", err)
		return 2
	}
	if cfg.ShowVersion {
		fmt.Println("flatpix", version)
		return 0
	}

	var h slog.Handler = slog.NewTextHandler(os.Stderr, nil)
	if cfg.LogJSON {
		h = slog.NewJSONHandler(os.Stderr, nil)
	}
	log := slog.New(h)
	slog.SetDefault(log)

	// One scratch directory for every temporary file, including the spool
	// files of libvips and of child tools. It is emptied now, at boot.
	sc, err := scratch.Open(cfg.ScratchDir, cfg.ScratchMB<<20)
	if err != nil {
		log.Error("scratch_failed", "err", err.Error())
		return 1
	}
	os.Setenv("TMPDIR", sc.Path())
	os.Setenv("MAGICK_TMPDIR", sc.Path())
	if os.Getenv("VIPS_BLOCK_UNTRUSTED") == "" {
		os.Setenv("VIPS_BLOCK_UNTRUSTED", "1") // no ImageMagick, SVG or PDF loaders inside libvips
	}
	if err := render.Startup(cfg.VipsConcurrency); err != nil {
		log.Error("libvips_failed", "err", err.Error())
		return 1
	}
	defer render.Shutdown()
	vips.SetStreamScratchDir(sc.Path())

	def, err := cfg.OpenOrigin(cfg.Origin)
	if err != nil {
		log.Error("origin_failed", "origin", cfg.Origin, "err", err.Error())
		return 1
	}
	clouds := map[string]origin.Origin{}
	for name, spec := range cfg.Clouds {
		o, err := cfg.OpenOrigin(spec)
		if err != nil {
			log.Error("origin_failed", "cloud", name, "err", err.Error())
			return 1
		}
		clouds[name] = o
	}
	derived, err := cfg.OpenCache()
	if err != nil {
		log.Error("cache_failed", "err", err.Error())
		return 1
	}
	opts := server.Options{
		Origins: origin.NewRouter(def, clouds), Cache: derived, Scratch: sc,
		Seats: cfg.Seats, Downloads: cfg.Downloads,
		MaxFileBytes: cfg.MaxFileBytes(), MaxInputPixels: cfg.MaxInputPixels, MaxOutputPixels: cfg.MaxPixels,
		QueueTimeout: cfg.QueueTimeout, DownloadTimeout: cfg.DownloadTimeout, RenderTimeout: cfg.RenderTimeout,
		Lenient: cfg.Lenient, Logger: log, Version: version,
	}
	if len(cfg.FetchAllow) > 0 {
		f, err := fetch.New(fetch.Options{Allow: cfg.FetchAllow, UserAgent: "Flatpix/" + version})
		if err != nil {
			log.Error("fetch_failed", "err", err.Error())
			return 1
		}
		opts.Fetch = f
	}
	web := render.Renderer(render.Vips{})
	if cfg.Isolate {
		exe, err := os.Executable()
		if err != nil {
			log.Error("isolate_failed", "err", err.Error())
			return 1
		}
		web = render.Child{Exe: exe, Env: sc.Env()}
	}
	opts.Renderer = &render.Dispatcher{Web: web} // print files: v0.2

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go sc.RunSweeper(ctx, 2*time.Minute, 10*time.Minute)

	srv := &http.Server{
		Addr: cfg.Listen, Handler: server.New(opts),
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 1 << 16,
	}
	log.Info("starting", append([]any{"version", version, "libvips", vips.Version}, cfg.Summary()...)...)
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		log.Error("listen_failed", "err", err.Error())
		return 1
	case <-ctx.Done():
	}
	log.Info("shutting_down", "grace", cfg.ShutdownTimeout.String())
	sctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		log.Warn("shutdown_incomplete", "err", err.Error())
		return 1
	}
	log.Info("stopped")
	return 0
}

// healthcheck is what the Docker HEALTHCHECK runs: GET /healthz on the
// listen port. The image has no curl.
func healthcheck(listen string) int {
	if listen == "" {
		listen = ":8080"
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", strings.TrimSpace(err.Error()))
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.StatusCode)
		return 1
	}
	return 0
}
