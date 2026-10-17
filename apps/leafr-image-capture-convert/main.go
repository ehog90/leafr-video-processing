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
	"path"
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
// Entries are reference-counted and removed once nobody holds or waits for them, so
// the map only tracks keys in use rather than every key ever requested.
type keyedLock struct {
	mu sync.Mutex
	m  map[string]*keyEntry
}

type keyEntry struct {
	ch   chan struct{}
	refs int // holders plus waiters; guarded by keyedLock.mu
}

func (k *keyedLock) acquire(ctx context.Context, key string) (func(), error) {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*keyEntry{}
	}
	e, ok := k.m[key]
	if !ok {
		e = &keyEntry{ch: make(chan struct{}, 1)}
		k.m[key] = e
	}
	e.refs++
	k.mu.Unlock()
	select {
	case e.ch <- struct{}{}:
		return func() {
			<-e.ch
			k.unref(key, e)
		}, nil
	case <-ctx.Done():
		k.unref(key, e)
		return nil, errf(409, "source_busy", "source %s is busy with another capture", key)
	}
}

func (k *keyedLock) unref(key string, e *keyEntry) {
	k.mu.Lock()
	defer k.mu.Unlock()
	e.refs--
	if e.refs == 0 {
		delete(k.m, key)
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
	if cfg.PowerLineFrequency != 50 && cfg.PowerLineFrequency != 60 {
		log.Fatalf("POWER_LINE_FREQUENCY must be 50 or 60, got %d", cfg.PowerLineFrequency)
	}
	if st, err := os.Stat(cfg.MediaDir); cfg.MediaDir == "" || err != nil || !st.IsDir() {
		log.Fatalf("MEDIA_DIR must be an existing directory, got %q", cfg.MediaDir)
	}
	wm, err := loadWatermark(cfg)
	if err != nil {
		log.Fatalf("watermark assets: %v", err)
	}
	sweepStale(cfg.TmpDir, 0)

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
	log.Printf("🚀 listening on %s", cfg.Addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// sweepStale removes leftovers of crashed runs: capture-* temp dirs older than minAge.
func sweepStale(root string, minAge time.Duration) {
	cutoff := time.Now().Add(-minAge)
	if m, _ := filepath.Glob(filepath.Join(root, "capture-*")); m != nil {
		for _, p := range m {
			if fi, err := os.Stat(p); err == nil && fi.ModTime().Before(cutoff) {
				os.RemoveAll(p)
			}
		}
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
		log.Printf("💥 internal error: %v", err)
		ae = &apiError{Status: 500, Code: "internal_error", Message: "internal error"}
	} else if ae.Status >= 500 {
		log.Printf("❌ capture failed: %s", ae)
	} else if ae.Status >= 400 {
		log.Printf("⚠️ request rejected: %s", ae)
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
	log.Printf("📦 capture payload: %s", req.loggable())
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
	imgOpt := (*ImageOptions)(nil)
	if req.Image != nil {
		imgOpt = req.Image
	}
	imgSettings := resolve(cfg.DefaultImage, imgOpt.formatOr(), imgOpt.webpOr())
	thumbOpt := (*ThumbnailOptions)(nil)
	if req.Thumbnail != nil {
		thumbOpt = req.Thumbnail
	}
	thumbSettings := resolve(cfg.DefaultThumb, thumbOpt.formatOr(), thumbOpt.webpOr())
	thumbOn := thumbOpt == nil || thumbOpt.Enabled == nil || *thumbOpt.Enabled
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
	if req.Source.Type != "local" {
		resp.CapturedResolution = strconv.Itoa(sw) + "x" + strconv.Itoa(sh)
	}

	// Outputs are written flat into MEDIA_DIR/converts/ as <id>_.<ext> and <id>_thumbnail.<ext>.
	// The consumer deletes them after it has stored them; a failed conversion removes them here.
	id := convertID(capturedAt)
	if err = os.MkdirAll(filepath.Join(cfg.MediaDir, "converts"), 0o755); err != nil {
		return nil, errf(500, "media_unavailable", "cannot create output directory: %v", err)
	}
	imgRel := path.Join("converts", id+"_."+outputExt(imgSettings.Format))
	outW, outH := outputSize(req.Source.Resolution)
	job := &convertJob{
		src: raw, tmpDir: tmp,
		mainOut:    filepath.Join(cfg.MediaDir, filepath.FromSlash(imgRel)),
		capturedAt: capturedAt, watermark: wmOn, wm: s.wm,
		outW: outW, outH: outH,
		image: imgSettings, thumb: thumbSettings,
	}
	job.thumbW, job.thumbH = thumbSize(outW, outH, thumbOpt.widthOr(outW))
	thumbRel := ""
	if thumbOn {
		thumbRel = path.Join("converts", id+"_thumbnail."+outputExt(thumbSettings.Format))
		job.thumbOut = filepath.Join(cfg.MediaDir, filepath.FromSlash(thumbRel))
	}
	// A failed conversion must not leave partial files in the media directory.
	defer func() {
		if err != nil {
			os.Remove(job.mainOut)
			if job.thumbOut != "" {
				os.Remove(job.thumbOut)
			}
		}
	}()
	if err = job.convert(ctx); err != nil {
		return nil, err
	}
	mainInfo, err := os.Stat(job.mainOut)
	if err != nil {
		return nil, errf(500, "media_unavailable", "cannot read converted image: %v", err)
	}
	var thumbBytes int64
	if job.thumbOut != "" {
		thumbInfo, err := os.Stat(job.thumbOut)
		if err != nil {
			return nil, errf(500, "media_unavailable", "cannot read converted thumbnail: %v", err)
		}
		thumbBytes = thumbInfo.Size()
	}
	// The raw frame is no longer needed; free tmpfs/disk before finishing.
	os.Remove(raw)

	resp.ImagePath = imgRel
	resp.Format = imgSettings.Format
	resp.Width, resp.Height, resp.Size = job.outW, job.outH, mainInfo.Size()
	if job.thumbOut != "" {
		resp.ThumbnailPath = thumbRel
		resp.ThumbnailFormat = thumbSettings.Format
		resp.ThumbnailWidth, resp.ThumbnailHeight, resp.ThumbnailSize = job.thumbW, job.thumbH, thumbBytes
	}

	resp.CapturedAt = capturedAt.Format(time.RFC3339)
	log.Printf("✅ captured source=%s frame=%dx%d output=%dx%d bytes=%d took=%s",
		req.Source.Type, sw, sh, job.outW, job.outH, mainInfo.Size(), time.Since(started).Round(time.Millisecond))
	return resp, nil
}

// convertID names the output files, for example 20261017-141500-1a2b3c4d.
func convertID(capturedAt time.Time) string {
	return capturedAt.Format("20060102-150405") + "-" + randID()
}
