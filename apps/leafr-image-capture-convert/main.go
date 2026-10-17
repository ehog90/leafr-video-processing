package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata"
)

type server struct {
	cfg   *Config
	wm    *watermark
	loc   *time.Location
	sem   chan struct{}
	locks keyedLock
}

// keyedLock serialises access per device/host while honouring context cancellation.
type keyedLock struct {
	mu sync.Mutex
	m  map[string]chan struct{}
}

func (k *keyedLock) acquire(ctx context.Context, key string) (func(), error) {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]chan struct{}{}
	}
	ch, ok := k.m[key]
	if !ok {
		ch = make(chan struct{}, 1)
		k.m[key] = ch
	}
	k.mu.Unlock()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, errf(409, "source_busy", "source %s is busy with another capture", key)
	}
}

func main() {
	cfg := loadConfig()
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		resp, err := http.Get("http://127.0.0.1" + cfg.Addr[strings.LastIndex(cfg.Addr, ":"):] + "/healthz")
		if err != nil || resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		os.Exit(0)
	}
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		log.Fatalf("invalid TIMEZONE %q: %v", cfg.Timezone, err)
	}
	wm, err := loadWatermark(cfg)
	if err != nil {
		log.Fatalf("watermark assets: %v", err)
	}
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		log.Fatalf("cannot create output dir: %v", err)
	}
	if err := probeWritable(cfg.OutputDir); err != nil {
		log.Fatalf("output dir %s is not writable by uid %d (on the host run: sudo chown -R 10001:10001 <host dir>): %v", cfg.OutputDir, os.Getuid(), err)
	}
	sweepStale(cfg.TmpDir, 0)
	sweepStale(cfg.OutputDir, 0)

	s := &server{cfg: cfg, wm: wm, loc: loc, sem: make(chan struct{}, cfg.MaxConcurrent)}
	go func() {
		for range time.Tick(10 * time.Minute) {
			sweepStale(cfg.TmpDir, 15*time.Minute)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /capture", s.handleCapture)
	srv := &http.Server{
		Addr: cfg.Addr, Handler: mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      cfg.Timeout + 20*time.Second,
	}
	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout+5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()
	log.Printf("listening on %s, output %s", cfg.Addr, cfg.OutputDir)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// probeWritable checks that the service user can create files in dir.
func probeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// sweepStale removes leftovers of crashed runs: capture-* temp dirs and *.part files.
func sweepStale(root string, minAge time.Duration) {
	cutoff := time.Now().Add(-minAge)
	if m, _ := filepath.Glob(filepath.Join(root, "capture-*")); m != nil {
		for _, p := range m {
			if fi, err := os.Stat(p); err == nil && fi.ModTime().Before(cutoff) {
				os.RemoveAll(p)
			}
		}
	}
	if root == os.Getenv("OUTPUT_DIR") || minAge == 0 {
		filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".part") {
				os.Remove(p)
			}
			return nil
		})
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	var ae *apiError
	if !errors.As(err, &ae) {
		log.Printf("internal error: %v", err)
		ae = &apiError{Status: 500, Code: "internal_error", Message: "internal error"}
	} else if ae.Status >= 500 {
		log.Printf("capture failed: %s", ae)
	}
	if errors.Is(err, errTimeout) {
		ae = &apiError{Status: 504, Code: "capture_timeout", Message: "operation timed out"}
	}
	writeJSON(w, ae.Status, map[string]any{"error": map[string]string{"code": ae.Code, "message": ae.Message}})
}

func randID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *server) handleCapture(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req CaptureRequest
	if err := dec.Decode(&req); err != nil {
		writeErr(w, bad("invalid JSON body: %v", err))
		return
	}
	if err := s.cfg.validate(&req); err != nil {
		writeErr(w, err)
		return
	}
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		w.Header().Set("Retry-After", "2")
		writeErr(w, errf(429, "too_many_requests", "too many concurrent captures"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()

	resp, err := s.process(ctx, &req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) process(ctx context.Context, req *CaptureRequest) (resp *CaptureResponse, err error) {
	started := time.Now()
	cfg := s.cfg
	wo := cfg.DefaultWebP
	if req.WebP != nil {
		wo = *req.WebP
	}
	to := cfg.DefaultThumb
	if req.Thumbnail != nil {
		to = *req.Thumbnail
	}
	thumbOn := req.Thumbnail == nil || req.Thumbnail.Enabled == nil || *req.Thumbnail.Enabled
	wmOn := req.Watermark == nil || req.Watermark.Enabled == nil || *req.Watermark.Enabled
	loc := s.loc
	if req.Watermark != nil && req.Watermark.Timezone != "" {
		loc, _ = time.LoadLocation(req.Watermark.Timezone) // validated
	}

	// The raw capture lives only in this directory, removed on every exit path.
	tmp, err := os.MkdirTemp(cfg.TmpDir, "capture-")
	if err != nil {
		return nil, errf(500, "tmp_unavailable", "cannot create temp dir: %v", err)
	}
	defer os.RemoveAll(tmp)

	resp = &CaptureResponse{Source: req.Source.Type}
	var raw string
	switch req.Source.Type {
	case "local":
		raw = filepath.Join(tmp, "raw.png")
		release, lerr := s.locks.acquire(ctx, req.Source.Device)
		if lerr != nil {
			return nil, lerr
		}
		res, cerr := cfg.captureLocal(ctx, req.Source, raw)
		release()
		if cerr != nil {
			return nil, cerr
		}
		resp.CapturedResolution = res
	case "rtsp":
		raw = filepath.Join(tmp, "raw.png")
		if err := cfg.captureRTSP(ctx, req.Source, raw); err != nil {
			return nil, err
		}
	case "gopro":
		raw = filepath.Join(tmp, "raw.jpg")
		release, lerr := s.locks.acquire(ctx, req.Source.goproAddr())
		if lerr != nil {
			return nil, lerr
		}
		cerr := cfg.captureGoPro(ctx, req.Source, raw)
		release()
		if cerr != nil {
			return nil, cerr
		}
	}
	capturedAt := time.Now().In(loc)

	sw, sh, err := imageSize(raw)
	if err != nil {
		return nil, err
	}
	if sw*sh > cfg.MaxPixels {
		return nil, errf(422, "image_too_large", "captured image is %dx%d, above the %d pixel limit", sw, sh, cfg.MaxPixels)
	}
	if req.Source.Type == "gopro" {
		resp.CapturedResolution = strconv.Itoa(sw) + "x" + strconv.Itoa(sh)
	}

	dir := cfg.OutputDir
	if req.Subdir != "" {
		dir = filepath.Join(dir, filepath.FromSlash(req.Subdir))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, errf(500, "output_unavailable", "cannot create output directory: %v", err)
	}
	prefix := req.Name
	if prefix == "" {
		prefix = req.Source.Type
	}
	base := prefix + "_" + capturedAt.UTC().Format("20060102T150405Z") + "_" + randID()
	mainFinal := filepath.Join(dir, base+".webp")
	thumbFinal := filepath.Join(dir, base+".thumb.webp")

	job := &convertJob{
		src: raw, tmpDir: tmp, mainOut: mainFinal + ".part",
		capturedAt: capturedAt, watermark: wmOn, wm: s.wm,
		webp: wo, thumbQ: to.Quality,
	}
	job.outW, job.outH = fit(sw, sh, wo.Width, wo.Height)
	if thumbOn {
		job.thumbOut = thumbFinal + ".part"
		job.thumbW, job.thumbH = fit(job.outW, job.outH, to.Width, to.Height)
	}
	defer func() { // never leave partial outputs behind
		os.Remove(job.mainOut)
		if job.thumbOut != "" {
			os.Remove(job.thumbOut)
		}
		if err != nil {
			os.Remove(mainFinal)
			os.Remove(thumbFinal)
		}
	}()
	if err = job.convert(ctx); err != nil {
		return nil, err
	}
	mainInfo, err := os.Stat(job.mainOut)
	if err != nil {
		return nil, errf(500, "output_unavailable", "cannot read converted image: %v", err)
	}
	var thumbSize int64
	if job.thumbOut != "" {
		thumbInfo, statErr := os.Stat(job.thumbOut)
		if statErr != nil {
			return nil, errf(500, "output_unavailable", "cannot read converted thumbnail: %v", statErr)
		}
		thumbSize = thumbInfo.Size()
	}
	// The raw frame is no longer needed; free tmpfs/disk before finishing.
	os.Remove(raw)

	if err = os.Rename(job.mainOut, mainFinal); err != nil {
		return nil, errf(500, "output_unavailable", "cannot write output: %v", err)
	}
	resp.Path, resp.Width, resp.Height, resp.Size = filepath.ToSlash(mainFinal), job.outW, job.outH, mainInfo.Size()
	if thumbOn {
		if err = os.Rename(job.thumbOut, thumbFinal); err != nil {
			return nil, errf(500, "output_unavailable", "cannot write thumbnail: %v", err)
		}
		resp.ThumbnailPath, resp.ThumbnailWidth, resp.ThumbnailHeight, resp.ThumbnailSize = filepath.ToSlash(thumbFinal), job.thumbW, job.thumbH, thumbSize
	}
	resp.CapturedAt = capturedAt.Format(time.RFC3339)
	log.Printf("captured source=%s %dx%d -> %s thumb=%s took=%s",
		req.Source.Type, job.outW, job.outH, resp.Path, resp.ThumbnailPath, time.Since(started).Round(time.Millisecond))
	return resp, nil
}
