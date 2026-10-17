package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestMediaResolve(t *testing.T) {
	m := testMedia(t)
	ok := map[string]string{
		"a/b.mp4":                             filepath.Join(m.root, "a", "b.mp4"),
		"./a/../b.mp4":                        filepath.Join(m.root, "b.mp4"),
		filepath.Join(m.root, "a", "b"):       filepath.Join(m.root, "a", "b"),
		filepath.Join(m.root, "x", "..", "y"): filepath.Join(m.root, "y"),
	}
	for in, want := range ok {
		got, err := m.resolve(in)
		if err != nil || got != want {
			t.Errorf("resolve(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "../x", "a/../../x", filepath.Dir(m.root), m.root, "a\x00b"} {
		if got, err := m.resolve(in); err == nil {
			t.Errorf("resolve(%q) = %q, want error", in, got)
		}
	}
	if got := m.relative(filepath.Join(m.root, "a", "b.mp4")); got != "a/b.mp4" {
		t.Errorf("relative = %q", got)
	}
}

func TestValidateRequest(t *testing.T) {
	m := testMedia(t)
	cfg := &Config{ThumbFormat: "webp", ThumbWidth: 600}
	base := func() CreateRequest {
		return CreateRequest{
			SequenceFile: "tmp/seq.imageseq", OutputFile: "out/t.mp4", Codec: "h264",
			Width: 1920, Height: 1080,
		}
	}

	req := base()
	spec, err := validateRequest(&req, m, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Codec != "H264" || spec.FPS != 30 || spec.Thumbnail != nil {
		t.Fatalf("defaults wrong: %+v", spec)
	}

	req = base()
	req.Thumbnail = &ThumbnailRequest{OutputFile: "out/thumb.webp"}
	spec, err = validateRequest(&req, m, cfg)
	if err != nil || spec.Thumbnail.Format != "webp" || spec.Thumbnail.Width != 600 {
		t.Fatalf("thumb defaults: %+v %v", spec, err)
	}

	bad := map[string]func(*CreateRequest){
		"codec":          func(r *CreateRequest) { r.Codec = "VP9" },
		"fps":            func(r *CreateRequest) { r.FPS = 25 },
		"size":           func(r *CreateRequest) { r.Width = 0 },
		"huge":           func(r *CreateRequest) { r.Height = 100000 },
		"no seq":         func(r *CreateRequest) { r.SequenceFile = "" },
		"outside seq":    func(r *CreateRequest) { r.SequenceFile = "../seq" },
		"outside out":    func(r *CreateRequest) { r.OutputFile = "../t.mp4" },
		"ext":            func(r *CreateRequest) { r.OutputFile = "out/t.avi" },
		"seq==out":       func(r *CreateRequest) { r.SequenceFile = "out/t.mp4" },
		"thumb no out":   func(r *CreateRequest) { r.Thumbnail = &ThumbnailRequest{} },
		"thumb format":   func(r *CreateRequest) { r.Thumbnail = &ThumbnailRequest{OutputFile: "t.gif", Format: "gif"} },
		"thumb ext":      func(r *CreateRequest) { r.Thumbnail = &ThumbnailRequest{OutputFile: "t.png"} },
		"thumb==out":     func(r *CreateRequest) { r.OutputFile = "t.mp4"; r.Thumbnail = &ThumbnailRequest{OutputFile: "t.mp4"} },
		"thumb width":    func(r *CreateRequest) { r.Thumbnail = &ThumbnailRequest{OutputFile: "t.webp", Width: 5} },
		"thumb escaping": func(r *CreateRequest) { r.Thumbnail = &ThumbnailRequest{OutputFile: "../t.webp"} },
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			r := base()
			mutate(&r)
			if _, err := validateRequest(&r, m, cfg); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestEncodeArgs(t *testing.T) {
	cfg := &Config{Threads: 4, AV1Preset: 4, AV1CRF: 32}
	spec := &Spec{OutputFile: "/media/o.mp4", Codec: "AV1", FPS: 30, Width: 1920, Height: 1080}
	args := strings.Join(encodeArgs(cfg, spec, "/tmp/l.ffconcat", "/media/o.mp4.partial", 12.5), " ")
	for _, want := range []string{
		"-f concat -safe 0 -i /tmp/l.ffconcat",
		"scale=w=1920:h=1080:force_original_aspect_ratio=decrease:force_divisible_by=2",
		"-pix_fmt yuv420p", "-r 30", "-t 12.500000",
		"-c:v libsvtav1 -preset 4 -crf 32 -svtav1-params lp=4",
		"-movflags +faststart -f mp4 -y /media/o.mp4.partial",
		"-progress pipe:1",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("missing %q in %s", want, args)
		}
	}

	spec = &Spec{OutputFile: "/media/o.mkv", Codec: "HEVC", FPS: 24, Width: 640, Height: 480}
	args = strings.Join(encodeArgs(cfg, spec, "l", "/media/o.mkv.partial", 5), " ")
	if !strings.Contains(args, "-c:v libx265 -tag:v hvc1") || !strings.Contains(args, "-f matroska") || strings.Contains(args, "faststart") {
		t.Errorf("hevc/mkv args: %s", args)
	}
}

func TestThumbArgs(t *testing.T) {
	cfg := &Config{ThumbFPS: 15, ThumbMaxSec: 3, ThumbWebPQuality: 70, ThumbAVIFCRF: 35}
	args, sec := thumbArgs(cfg, &ThumbSpec{Format: "webp", Width: 600}, "in", "out", 30)
	joined := strings.Join(args, " ")
	if sec != 3 || !strings.Contains(joined, "setpts=PTS*0.100000000,fps=15,scale='min(600,iw)':-2") ||
		!strings.Contains(joined, "-c:v libwebp -quality 70") || !strings.Contains(joined, "-loop 0 -f webp") {
		t.Errorf("webp (%v): %s", sec, joined)
	}

	args, sec = thumbArgs(cfg, &ThumbSpec{Format: "avif", Width: 300}, "in", "out", 2)
	joined = strings.Join(args, " ")
	if sec != 2 || strings.Contains(joined, "setpts") || !strings.Contains(joined, "-c:v libsvtav1") || !strings.Contains(joined, "-f avif") {
		t.Errorf("avif (%v): %s", sec, joined)
	}
}
