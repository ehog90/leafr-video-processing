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

// WebPSettings are the fully resolved WebP encoder settings for one output.
type WebPSettings struct {
	Quality          int
	Lossless         bool
	CompressionLevel int
	Preset           string
}

// WebPOptions are request overrides for WebPSettings. Omitted fields keep the configured default.
type WebPOptions struct {
	Quality          *int    `json:"quality,omitempty"`           // 0-100
	Lossless         *bool   `json:"lossless,omitempty"`          // default false
	CompressionLevel *int    `json:"compression_level,omitempty"` // 0-6
	Preset           *string `json:"preset,omitempty"`            // default|picture|photo|drawing|icon|text
}

func (o *WebPOptions) apply(base WebPSettings) WebPSettings {
	if o == nil {
		return base
	}
	if o.Quality != nil {
		base.Quality = *o.Quality
	}
	if o.Lossless != nil {
		base.Lossless = *o.Lossless
	}
	if o.CompressionLevel != nil {
		base.CompressionLevel = *o.CompressionLevel
	}
	if o.Preset != nil {
		base.Preset = *o.Preset
	}
	return base
}

// OutputSettings is the resolved encoding for one output (the image or the thumbnail).
type OutputSettings struct {
	Format string
	WebP   WebPSettings
}

// resolve applies a request override to the configured defaults for one output.
func resolve(base OutputSettings, format string, wo *WebPOptions) OutputSettings {
	if format != "" {
		base.Format = format
	}
	base.WebP = wo.apply(base.WebP)
	return base
}

// ImageOptions overrides the encoding of the full-size image.
type ImageOptions struct {
	Format string       `json:"format,omitempty"` // default IMAGE_FORMAT; supported: webp
	WebP   *WebPOptions `json:"webp,omitempty"`   // used when format is webp
}

func (o *ImageOptions) formatOr() string {
	if o == nil {
		return ""
	}
	return o.Format
}

func (o *ImageOptions) webpOr() *WebPOptions {
	if o == nil {
		return nil
	}
	return o.WebP
}

// ThumbnailOptions overrides the encoding of the thumbnail, separately from the image.
type ThumbnailOptions struct {
	Enabled *bool        `json:"enabled,omitempty"` // default true
	Format  string       `json:"format,omitempty"`  // default THUMB_FORMAT; supported: webp
	WebP    *WebPOptions `json:"webp,omitempty"`    // used when format is webp
}

func (o *ThumbnailOptions) formatOr() string {
	if o == nil {
		return ""
	}
	return o.Format
}

func (o *ThumbnailOptions) webpOr() *WebPOptions {
	if o == nil {
		return nil
	}
	return o.WebP
}

type WatermarkOptions struct {
	Enabled  *bool  `json:"enabled,omitempty"`  // default true
	Timezone string `json:"timezone,omitempty"` // IANA name, default TIMEZONE env
}

type CaptureRequest struct {
	Source    Source            `json:"source"`
	Image     *ImageOptions     `json:"image,omitempty"`
	Thumbnail *ThumbnailOptions `json:"thumbnail,omitempty"`
	Watermark *WatermarkOptions `json:"watermark,omitempty"`
}

// CaptureResponse always contains the images as base64. Nothing is stored on the server.
type CaptureResponse struct {
	Image              string `json:"image"`
	Thumbnail          string `json:"thumbnail,omitempty"`
	Format             string `json:"format"`
	ThumbnailFormat    string `json:"thumbnail_format,omitempty"`
	Width              int    `json:"width"`
	Height             int    `json:"height"`
	Size               int64  `json:"size"`
	ThumbnailWidth     int    `json:"thumbnail_width,omitempty"`
	ThumbnailHeight    int    `json:"thumbnail_height,omitempty"`
	ThumbnailSize      int64  `json:"thumbnail_size,omitempty"`
	Source             string `json:"source"`
	CapturedAt         string `json:"captured_at"`
	CapturedResolution string `json:"captured_resolution,omitempty"`
}

var (
	devicePattern = regexp.MustCompile(`^/dev/video[0-9]{1,3}$`)
	resPattern    = regexp.MustCompile(`^[0-9]{2,5}x[0-9]{2,5}$`)
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

// validateOutput checks the format of one output and the options block for it.
func validateOutput(name, format string, wo *WebPOptions) error {
	if format != "" {
		if _, ok := supportedFormats[format]; !ok {
			return bad("%s.format must be one of: %s", name, supportedFormatList())
		}
	}
	if wo == nil {
		return nil
	}
	if q := wo.Quality; q != nil && (*q < 0 || *q > 100) {
		return bad("%s.webp.quality must be 0-100", name)
	}
	if c := wo.CompressionLevel; c != nil && (*c < 0 || *c > 6) {
		return bad("%s.webp.compression_level must be 0-6", name)
	}
	if p := wo.Preset; p != nil {
		switch *p {
		case "", "default", "picture", "photo", "drawing", "icon", "text":
		default:
			return bad("%s.webp.preset must be default|picture|photo|drawing|icon|text", name)
		}
	}
	return nil
}

func supportedFormatList() string {
	list := ""
	for f := range supportedFormats {
		if list != "" {
			list += "|"
		}
		list += f
	}
	return list
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
	if i := r.Image; i != nil {
		if err := validateOutput("image", i.Format, i.WebP); err != nil {
			return err
		}
	}
	if t := r.Thumbnail; t != nil {
		if err := validateOutput("thumbnail", t.Format, t.WebP); err != nil {
			return err
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
