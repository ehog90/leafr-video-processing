package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr           string
	OutputDir      string
	TmpDir         string
	FontFile       string
	LogoFile       string
	Timezone       string
	AllowedDevices map[string]bool // empty = any /dev/videoN
	MaxConcurrent  int
	Timeout        time.Duration
	DefaultSkip    int
	MaxPixels      int // reject captured frames larger than this
	DefaultWebP    WebPOptions
	DefaultThumb   ThumbnailOptions
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

func loadConfig() *Config {
	c := &Config{
		Addr:          env("LISTEN_ADDR", ":8080"),
		OutputDir:     env("OUTPUT_DIR", "/data/images"),
		TmpDir:        env("TMP_DIR", os.TempDir()),
		FontFile:      env("FONT_FILE", "/usr/share/leafr/Manrope-SemiBold.ttf"),
		LogoFile:      env("LOGO_FILE", "/usr/share/leafr/logo.png"),
		Timezone:      env("TIMEZONE", "Europe/Budapest"),
		MaxConcurrent: envInt("MAX_CONCURRENT", 2),
		Timeout:       time.Duration(envInt("CAPTURE_TIMEOUT_SECONDS", 40)) * time.Second,
		DefaultSkip:   envInt("FSWEBCAM_SKIP_FRAMES", 5),
		MaxPixels:     envInt("MAX_PIXELS", 64_000_000),
		DefaultWebP: WebPOptions{
			Width:            envInt("WEBP_WIDTH", 0),
			Height:           envInt("WEBP_HEIGHT", 0),
			Quality:          envInt("WEBP_QUALITY", 80),
			CompressionLevel: envInt("WEBP_COMPRESSION_LEVEL", 4),
			Preset:           env("WEBP_PRESET", "default"),
		},
		DefaultThumb: ThumbnailOptions{
			Width:   envInt("THUMB_WIDTH", 320),
			Height:  envInt("THUMB_HEIGHT", 0),
			Quality: envInt("THUMB_QUALITY", 70),
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
