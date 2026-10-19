package main

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Config holds the process-wide settings, all read from environment variables.
type Config struct {
	Addr             string
	MediaDir         string
	TmpDir           string
	FFmpegBin        string
	Threads          int // encoder threads per ffmpeg process
	AV1Preset        int // libsvtav1 preset (0 slowest .. 13 fastest)
	AV1CRF           int
	ThumbFormat      string // default animated thumbnail format: webp | avif
	ThumbWidth       int    // default maximum thumbnail width in px
	ThumbFPS         int
	ThumbMaxSec      float64 // thumbnails longer than this are sped up to this length
	ThumbWebPQuality int
	ThumbAVIFCRF     int
	JobRetention     time.Duration

	RedisAddr      string
	RedisPassword  string
	RedisDB        int
	RedisKeyPrefix string
	// ClaimIdle is how long a queued job may stay unattended by the replica that took it
	// before another replica treats that replica as dead.
	ClaimIdle time.Duration
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

func envFloat(k string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil {
		return v
	}
	return def
}

func loadConfig() *Config {
	c := &Config{
		Addr:             env("LISTEN_ADDR", ":8090"),
		MediaDir:         env("MEDIA_DIR", "/media"),
		TmpDir:           env("TMP_DIR", os.TempDir()),
		FFmpegBin:        env("FFMPEG_BIN", "ffmpeg"),
		Threads:          envInt("ENCODER_THREADS", runtime.NumCPU()),
		AV1Preset:        envInt("AV1_PRESET", 4),
		AV1CRF:           envInt("AV1_CRF", 32),
		ThumbFormat:      strings.ToLower(env("THUMB_FORMAT", "webp")),
		ThumbWidth:       envInt("THUMB_WIDTH", 600),
		ThumbFPS:         envInt("THUMB_FPS", 15),
		ThumbMaxSec:      envFloat("THUMB_MAX_SECONDS", 3),
		ThumbWebPQuality: envInt("THUMB_WEBP_QUALITY", 70),
		ThumbAVIFCRF:     envInt("THUMB_AVIF_CRF", 35),
		JobRetention:     time.Duration(envInt("JOB_RETENTION_MINUTES", 60)) * time.Minute,
		RedisAddr:        env("REDIS_ADDR", "127.0.0.1:6379"),
		RedisPassword:    os.Getenv("REDIS_PASSWORD"),
		RedisDB:          envInt("REDIS_DB", 0),
		RedisKeyPrefix:   env("REDIS_KEY_PREFIX", "leafr:timelapse"),
		ClaimIdle:        time.Duration(envInt("JOB_CLAIM_IDLE_SECONDS", 60)) * time.Second,
	}
	if c.ClaimIdle < 5*time.Second {
		c.ClaimIdle = 5 * time.Second
	}
	if c.Threads < 1 {
		c.Threads = 1
	}
	if c.ThumbFPS < 1 {
		c.ThumbFPS = 15
	}
	if c.ThumbMaxSec <= 0 {
		c.ThumbMaxSec = 3
	}
	return c
}
