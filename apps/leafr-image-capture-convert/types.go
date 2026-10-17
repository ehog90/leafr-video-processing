package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Source describes where the frame comes from. Which fields apply depends on Type.
type Source struct {
	Type string `json:"type"` // "local" | "rtsp" | "gopro"

	// local
	Device     string `json:"device,omitempty"`     // /dev/videoN
	Resolution string `json:"resolution,omitempty"` // WxH, default: maximal supported
	SkipFrames *int   `json:"skip_frames,omitempty"`

	// rtsp
	URL      string `json:"url,omitempty"` // rtsp:// or rtsps:// without credentials
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`

	// gopro (Open GoPro HTTP API)
	Host               string `json:"host,omitempty"` // private IP, default 10.5.5.9
	Port               int    `json:"port,omitempty"` // default 8080
	SetPhotoMode       *bool  `json:"set_photo_mode,omitempty"`
	DeleteAfterCapture bool   `json:"delete_after_capture,omitempty"`
}

type WebPOptions struct {
	Width            int    `json:"width"`
	Height           int    `json:"height"`
	Quality          int    `json:"quality"`
	Lossless         bool   `json:"lossless"`
	CompressionLevel int    `json:"compression_level"`
	Preset           string `json:"preset,omitempty"`
}

type ThumbnailOptions struct {
	Enabled *bool `json:"enabled,omitempty"`
	Width   int   `json:"width"`
	Height  int   `json:"height"`
	Quality int   `json:"quality"`
}

type WatermarkOptions struct {
	Enabled  *bool  `json:"enabled,omitempty"`  // default true
	Timezone string `json:"timezone,omitempty"` // IANA name, default TIMEZONE env
}

type CaptureRequest struct {
	Source    Source            `json:"source"`
	WebP      *WebPOptions      `json:"webp,omitempty"`
	Thumbnail *ThumbnailOptions `json:"thumbnail,omitempty"`
	Watermark *WatermarkOptions `json:"watermark,omitempty"`
	Subdir    string            `json:"subdir,omitempty"`
	Name      string            `json:"name,omitempty"`
}

type CaptureResponse struct {
	Path               string `json:"path"`
	Width              int    `json:"width"`
	Height             int    `json:"height"`
	ThumbnailPath      string `json:"thumbnail_path,omitempty"`
	ThumbnailWidth     int    `json:"thumbnail_width,omitempty"`
	ThumbnailHeight    int    `json:"thumbnail_height,omitempty"`
	Source             string `json:"source"`
	CapturedAt         string `json:"captured_at"`
	CapturedResolution string `json:"captured_resolution,omitempty"`
}

var (
	devicePattern = regexp.MustCompile(`^/dev/video[0-9]{1,3}$`)
	resPattern    = regexp.MustCompile(`^[0-9]{2,5}x[0-9]{2,5}$`)
	namePattern   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	subdirPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+(/[A-Za-z0-9_-]+)*$`)
)

func bad(format string, a ...any) error {
	return &apiError{Status: 400, Code: "invalid_request", Message: fmt.Sprintf(format, a...)}
}

func (s Source) goproAddr() string {
	host, port := s.Host, s.Port
	if host == "" {
		host = "10.5.5.9"
	}
	if port == 0 {
		port = 8080
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func (c *Config) validate(r *CaptureRequest) error {
	switch r.Source.Type {
	case "local":
		if !devicePattern.MatchString(r.Source.Device) {
			return bad("source.device must match /dev/videoN")
		}
		if len(c.AllowedDevices) > 0 && !c.AllowedDevices[r.Source.Device] {
			return &apiError{Status: 403, Code: "device_not_allowed", Message: fmt.Sprintf("device %s is not allowed", r.Source.Device)}
		}
		if r.Source.Resolution != "" && !resPattern.MatchString(r.Source.Resolution) {
			return bad("source.resolution must look like 1920x1080")
		}
		if s := r.Source.SkipFrames; s != nil && (*s < 0 || *s > 100) {
			return bad("source.skip_frames must be 0-100")
		}
	case "rtsp":
		u, err := url.Parse(r.Source.URL)
		if err != nil || (u.Scheme != "rtsp" && u.Scheme != "rtsps") || u.Host == "" {
			return bad("source.url must be a valid rtsp:// or rtsps:// URL")
		}
		if u.User != nil {
			return bad("put credentials in source.username/password, not in the URL")
		}
		if (r.Source.Username == "") != (r.Source.Password == "") {
			return bad("source.username and source.password must be given together")
		}
	case "gopro":
		if h := r.Source.Host; h != "" {
			ip := net.ParseIP(h)
			if ip == nil || !(ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
				return bad("source.host must be a private/link-local IP address (GoPro default 10.5.5.9)")
			}
		}
		if p := r.Source.Port; p != 0 && (p < 1 || p > 65535) {
			return bad("source.port must be 1-65535")
		}
	default:
		return bad(`source.type must be "local", "rtsp" or "gopro"`)
	}
	if r.Name != "" && !namePattern.MatchString(r.Name) {
		return bad("name must match [A-Za-z0-9_-]{1,64}")
	}
	if r.Subdir != "" && (len(r.Subdir) > 128 || !subdirPattern.MatchString(r.Subdir)) {
		return bad("subdir must be slash-separated [A-Za-z0-9_-] segments")
	}
	if w := r.WebP; w != nil {
		if w.Width < 0 || w.Height < 0 || w.Width > 16384 || w.Height > 16384 {
			return bad("webp.width/height must be 0-16384")
		}
		if w.Quality < 0 || w.Quality > 100 {
			return bad("webp.quality must be 0-100")
		}
		if w.CompressionLevel < 0 || w.CompressionLevel > 6 {
			return bad("webp.compression_level must be 0-6")
		}
		switch w.Preset {
		case "", "default", "picture", "photo", "drawing", "icon", "text":
		default:
			return bad("webp.preset must be default|picture|photo|drawing|icon|text")
		}
	}
	if t := r.Thumbnail; t != nil {
		if t.Width < 0 || t.Height < 0 || t.Width > 16384 || t.Height > 16384 {
			return bad("thumbnail.width/height must be 0-16384")
		}
		if t.Quality < 0 || t.Quality > 100 {
			return bad("thumbnail.quality must be 0-100")
		}
	}
	if w := r.Watermark; w != nil && w.Timezone != "" {
		if _, err := time.LoadLocation(w.Timezone); err != nil {
			return bad("watermark.timezone is not a known IANA zone")
		}
	}
	return nil
}

func sanitize(s string, secrets ...string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "***")
		}
	}
	return s
}

// apiError is returned to clients as {"error":{"code","message"}}.
type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string { return e.Code + ": " + e.Message }

func errf(status int, code, format string, a ...any) error {
	return &apiError{Status: status, Code: code, Message: fmt.Sprintf(format, a...)}
}

var errTimeout = errors.New("timeout")
