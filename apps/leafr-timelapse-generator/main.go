package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	cfg := loadConfig()
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		client := http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get("http://127.0.0.1" + cfg.Addr[strings.LastIndex(cfg.Addr, ":"):] + "/healthz")
		if err != nil || resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if cfg.ThumbFormat != "webp" && cfg.ThumbFormat != "avif" {
		log.Fatalf("THUMB_FORMAT must be webp or avif, got %q", cfg.ThumbFormat)
	}
	media, err := newMediaDir(cfg.MediaDir)
	if err != nil {
		log.Fatalf("MEDIA_DIR %q: %v", cfg.MediaDir, err)
	}

	p := &pipeline{cfg: cfg, media: media}
	mgr := newManager(cfg, p.run)
	s := &server{cfg: cfg, media: media, mgr: mgr}

	srv := &http.Server{
		Addr: cfg.Addr, Handler: s.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
		<-stop
		log.Printf("⏹️ shutting down")
		// Cancels running jobs (their partial files are removed) and disconnects WebSockets.
		mgr.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	log.Printf("🎞️ leafr-timelapse-generator listening on %s (media: %s)", cfg.Addr, media.root)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-done
}
