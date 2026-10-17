package main

import (
	"slices"
	"strings"
	"testing"
)

func TestRTSPArgsGrabsStreamSizeAndWritesOutputLast(t *testing.T) {
	args := rtspArgs("rtsp://cam/ch1", "out.png")
	if slices.Contains(args, "-vf") {
		t.Fatalf("rtsp capture must not scale; conversion does it, got %v", args)
	}
	if args[len(args)-1] != "out.png" {
		t.Fatalf("output path must be last, got %v", args)
	}
}

func TestParseResolution(t *testing.T) {
	if w, h, ok := parseResolution("1920x1080"); !ok || w != 1920 || h != 1080 {
		t.Fatalf("1920x1080 parsed as %d %d %v", w, h, ok)
	}
	for _, bad := range []string{"", "1080p", "0x720", "00x00"} {
		if _, _, ok := parseResolution(bad); ok {
			t.Fatalf("%q must not parse", bad)
		}
	}
}

func TestLetterboxUsesRequestedSize(t *testing.T) {
	g := letterboxGraph("b0", 1920, 1080)
	if !strings.Contains(g, "scale=1920:1080:") || !strings.Contains(g, "pad=1920:1080:") {
		t.Fatalf("letterbox does not target 1920x1080: %s", g)
	}
}

func TestLoggableRedactsPassword(t *testing.T) {
	req := CaptureRequest{Source: Source{Type: "rtsp", URL: "rtsp://192.168.1.46:8554/ch1", Username: "772", Password: "s3cret"}}
	got := req.loggable()
	if strings.Contains(got, "s3cret") {
		t.Fatalf("password leaked into log line: %s", got)
	}
	if !strings.Contains(got, `"password":"[REDACTED]"`) || !strings.Contains(got, `"username":"772"`) {
		t.Fatalf("unexpected log line: %s", got)
	}
	if req.Source.Password != "s3cret" {
		t.Fatalf("loggable must not modify the request")
	}
}

func TestLoggableRedactsPasswordInURL(t *testing.T) {
	secret := "s3cret"
	req := CaptureRequest{Source: Source{Type: "rtsp", URL: "rtsp://user:" + secret + "@192.168.1.46/ch1"}}
	got := req.loggable()
	if strings.Contains(got, secret) {
		t.Fatalf("URL password leaked into log line: %s", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Fatalf("expected the URL password to be replaced, got %s", got)
	}
}

func TestValidateResolutionAppliesToRTSP(t *testing.T) {
	c := &Config{MaxPixels: 64_000_000}
	ok := CaptureRequest{Source: Source{Type: "rtsp", URL: "rtsp://cam/ch1", Resolution: "1920x1080"}}
	if err := c.validate(&ok); err != nil {
		t.Fatalf("valid rtsp resolution rejected: %v", err)
	}
	bad := CaptureRequest{Source: Source{Type: "rtsp", URL: "rtsp://cam/ch1", Resolution: "1080p"}}
	if err := c.validate(&bad); err == nil {
		t.Fatalf("invalid rtsp resolution accepted")
	}
	huge := CaptureRequest{Source: Source{Type: "rtsp", URL: "rtsp://cam/ch1", Resolution: "99999x99999"}}
	if err := c.validate(&huge); err == nil {
		t.Fatalf("resolution above MAX_PIXELS accepted")
	}
}
