package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// run executes a command and returns its combined output (secrets redacted, capped).
func run(ctx context.Context, secrets []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	msg := sanitize(out.String(), secrets...)
	if len(msg) > 2000 {
		msg = msg[len(msg)-2000:]
	}
	msg = strings.TrimSpace(msg)
	if err == nil {
		return msg, nil
	}
	if ctx.Err() != nil {
		return msg, errTimeout
	}
	if errors.Is(err, exec.ErrNotFound) {
		return msg, errf(500, "tool_missing", "%s is not installed", name)
	}
	return msg, fmt.Errorf("%s failed: %v", name, err)
}

var sizeRe = regexp.MustCompile(`Size:\s+(?:Discrete\s+(\d+)x(\d+)|Stepwise\s+\d+x\d+\s+-\s+(\d+)x(\d+))`)

// maxResolution returns the largest frame size the V4L2 device supports.
func maxResolution(ctx context.Context, device string) (string, error) {
	out, err := run(ctx, nil, "v4l2-ctl", "--device", device, "--list-formats-ext")
	if err != nil {
		return "", classifyV4L(out, err)
	}
	bestArea, best := 0, ""
	for _, m := range sizeRe.FindAllStringSubmatch(out, -1) {
		ws, hs := m[1], m[2]
		if ws == "" {
			ws, hs = m[3], m[4]
		}
		w, _ := strconv.Atoi(ws)
		h, _ := strconv.Atoi(hs)
		if w*h > bestArea {
			bestArea, best = w*h, ws+"x"+hs
		}
	}
	if best == "" {
		return "", errf(502, "device_no_formats", "%s reported no frame sizes (is it a capture device?)", device)
	}
	return best, nil
}

func classifyV4L(out string, err error) error {
	if errors.Is(err, errTimeout) {
		return errf(504, "capture_timeout", "camera did not respond in time")
	}
	var ae *apiError
	if errors.As(err, &ae) {
		return err
	}
	l := strings.ToLower(out)
	switch {
	case strings.Contains(l, "no such file") || strings.Contains(l, "cannot open"):
		if strings.Contains(l, "permission denied") {
			return errf(403, "device_permission_denied", "no permission to open the camera device")
		}
		return errf(404, "device_not_found", "camera device does not exist (not mapped into the container?)")
	case strings.Contains(l, "permission denied"):
		return errf(403, "device_permission_denied", "no permission to open the camera device (check group_add video)")
	case strings.Contains(l, "operation not permitted"):
		return errf(403, "device_permission_denied", "camera device open was not permitted (device mapped read-only, or cgroup device rule missing)")
	case strings.Contains(l, "busy"):
		return errf(409, "device_busy", "camera device is busy")
	}
	return errf(502, "camera_error", "camera error: %s", lastLine(out))
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// captureLocal grabs one frame with fswebcam: no banner/overlay, maximal resolution,
// lossless PNG (fswebcam's JPEG cannot reach a true 100% quality).
func (c *Config) captureLocal(ctx context.Context, s Source, dst string) (string, error) {
	if _, err := os.Stat(s.Device); err != nil {
		return "", errf(404, "device_not_found", "camera device %s does not exist (not mapped into the container?)", s.Device)
	}
	res := s.Resolution
	if res == "" {
		var err error
		if res, err = maxResolution(ctx, s.Device); err != nil {
			return "", err
		}
	}
	skip := c.DefaultSkip
	if s.SkipFrames != nil {
		skip = *s.SkipFrames
	}
	out, err := run(ctx, nil, "fswebcam",
		"--device", s.Device,
		"--resolution", res,
		"--skip", strconv.Itoa(skip),
		"--no-banner", "--no-overlay",
		"--png", "0",
		"--save", dst,
	)
	if err != nil {
		return "", classifyV4L(out, err)
	}
	if fi, err := os.Stat(dst); err != nil || fi.Size() == 0 {
		return "", errf(502, "camera_error", "fswebcam produced no image: %s", lastLine(out))
	}
	return res, nil
}

func (c *Config) captureRTSP(ctx context.Context, s Source, dst string) error {
	u, err := url.Parse(s.URL)
	if err != nil {
		return bad("invalid rtsp url")
	}
	var secrets []string
	if s.Username != "" {
		u.User = url.UserPassword(s.Username, s.Password)
		secrets = []string{s.Password, url.QueryEscape(s.Password), url.PathEscape(s.Password)}
	}
	out, err := run(ctx, secrets, "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin",
		"-rtsp_transport", "tcp",
		"-i", u.String(),
		"-frames:v", "1", "-an", "-y", dst)
	if err != nil {
		return classifyRTSP(out, err)
	}
	return nil
}

func classifyRTSP(out string, err error) error {
	if errors.Is(err, errTimeout) {
		return errf(504, "capture_timeout", "RTSP stream did not deliver a frame in time")
	}
	var ae *apiError
	if errors.As(err, &ae) {
		return err
	}
	l := strings.ToLower(out)
	switch {
	case strings.Contains(l, "401") || strings.Contains(l, "unauthorized"):
		return errf(502, "rtsp_unauthorized", "RTSP server rejected the credentials")
	case strings.Contains(l, "404") || strings.Contains(l, "not found"):
		return errf(502, "rtsp_stream_not_found", "RTSP stream path not found")
	case strings.Contains(l, "connection refused") || strings.Contains(l, "no route") ||
		strings.Contains(l, "timed out") || strings.Contains(l, "name or service not known") ||
		strings.Contains(l, "network is unreachable"):
		return errf(502, "rtsp_unreachable", "RTSP server is unreachable")
	}
	return errf(502, "rtsp_error", "RTSP capture failed: %s", lastLine(out))
}
