package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr               string
	TmpDir             string
	MediaDir           string // shared media directory; converted images are written to converts/
	FontFile           string
	LogoFile           string
	Timezone           string
	PowerLineFrequency int             // 50 or 60 Hz; sets the V4L2 power_line_frequency control for local cameras
	AllowedDevices     map[string]bool // empty = any /dev/videoN
	MaxConcurrent      int
	Timeout            time.Duration
	DefaultSkip        int
	MaxPixels          int // reject captured frames larger than this
	DefaultImage       OutputSettings
	DefaultThumb       OutputSettings
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func envBool(k string, def bool) bool {
	if v, err := strconv.ParseBool(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func loadConfig() *Config {
	c := &Config{
		Addr:               env("LISTEN_ADDR", ":8080"),
		TmpDir:             env("TMP_DIR", os.TempDir()),
		MediaDir:           env("MEDIA_DIR", ""),
		FontFile:           env("FONT_FILE", "/usr/share/leafr/Manrope-SemiBold.ttf"),
		LogoFile:           env("LOGO_FILE", "/usr/share/leafr/logo.png"),
		Timezone:           env("TIMEZONE", "Europe/Budapest"),
		PowerLineFrequency: envInt("POWER_LINE_FREQUENCY", 50),
		MaxConcurrent:      envInt("MAX_CONCURRENT", 2),
		Timeout:            time.Duration(envInt("CAPTURE_TIMEOUT_SECONDS", 40)) * time.Second,
		DefaultSkip:        envInt("FSWEBCAM_SKIP_FRAMES", 5),
		MaxPixels:          envInt("MAX_PIXELS", 64_000_000),
		DefaultImage: OutputSettings{
			Format: env("IMAGE_FORMAT", "webp"),
			WebP: WebPSettings{
				Quality:          envInt("WEBP_QUALITY", 80),
				Lossless:         envBool("WEBP_LOSSLESS", false),
				CompressionLevel: envInt("WEBP_COMPRESSION_LEVEL", 4),
				Preset:           env("WEBP_PRESET", "default"),
			},
		},
		DefaultThumb: OutputSettings{
			Format: env("THUMB_FORMAT", "webp"),
			WebP: WebPSettings{
				Quality:          envInt("THUMB_QUALITY", 70),
				Lossless:         envBool("THUMB_LOSSLESS", false),
				CompressionLevel: envInt("THUMB_COMPRESSION_LEVEL", 4),
				Preset:           env("THUMB_PRESET", "default"),
			},
		},
		AllowedDevices: map[string]bool{},
	}
	if c.MaxConcurrent < 1 {
		c.MaxConcurrent = 1
	}
	for _, d := range strings.Split(os.Getenv("ALLOWED_DEVICES"), ",") {
		if d = strings.TrimSpace(d); d != "" {
			c.AllowedDevices[d] = true
		}
	}
	return c
}
