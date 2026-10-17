package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
)

// logoAspect is the viewBox ratio of leafr_ver3_white.svg.
const logoAspect = 450.0 / 259.26

// imageSize reads only the header of the file, so no pixel data is held in memory.
func imageSize(path string) (int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0, errf(502, "invalid_capture", "captured data is not a valid image: %v", err)
	}
	return cfg.Width, cfg.Height, nil
}

// fit returns the output size for the requested bounds: 0 = derive from the other
// bound, both = fit inside keeping aspect ratio; never upscales.
func fit(sw, sh, w, h int) (int, int) {
	if w <= 0 && h <= 0 {
		return sw, sh
	}
	scale := math.MaxFloat64
	if w > 0 {
		scale = float64(w) / float64(sw)
	}
	if h > 0 {
		scale = math.Min(scale, float64(h)/float64(sh))
	}
	if scale >= 1 {
		return sw, sh
	}
	return max(1, int(math.Round(float64(sw)*scale))), max(1, int(math.Round(float64(sh)*scale)))
}

type watermark struct {
	face     font.Face
	fontFile string
	logoFile string
}

func loadWatermark(c *Config) (*watermark, error) {
	data, err := os.ReadFile(c.FontFile)
	if err != nil {
		return nil, fmt.Errorf("font: %w", err)
	}
	if _, err := os.Stat(c.LogoFile); err != nil {
		return nil, fmt.Errorf("logo: %w", err)
	}
	fnt, err := opentype.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("font: %w", err)
	}
	// Measuring face at a unit size; widths scale linearly with the font size.
	face, err := opentype.NewFace(fnt, &opentype.FaceOptions{Size: 100, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		return nil, err
	}
	return &watermark{face: face, fontFile: c.FontFile, logoFile: c.LogoFile}, nil
}

func (w *watermark) textWidth(text string, fontSize int) int {
	adv := font.MeasureString(w.face, text)
	return int(math.Ceil(float64(adv.Ceil()) * float64(fontSize) / 100))
}

// filterGraph builds the ffmpeg -filter_complex graph. Output pad [main] is the
// watermarked full image; [thumb] is added when thumbnail dimensions are given.
func (w *watermark) filterGraph(outW, outH int, stamp, textFile string, thumbW, thumbH int) string {
	var g []string
	g = append(g, fmt.Sprintf("[0:v]scale=%d:%d:flags=lanczos[b0]", outW, outH))
	last := "b0"

	fs := max(11, int(math.Round(float64(outW)*0.0165)))
	lh := int(math.Round(float64(fs) * 1.7))
	lw := int(math.Round(float64(lh) * logoAspect))
	gap := int(math.Round(float64(fs) * 0.5))
	pad := int(math.Round(float64(fs) * 0.55))
	margin := int(math.Round(float64(fs) * 0.9))
	tw := w.textWidth(stamp, fs)
	boxW := lw + gap + tw + 2*pad
	boxH := lh + 2*pad
	bx, by := outW-margin-boxW, outH-margin-boxH

	if bx >= 0 && by >= 0 {
		g = append(g,
			fmt.Sprintf("[1:v]scale=%d:%d:flags=lanczos[lg]", lw, lh),
			fmt.Sprintf("[%s]drawbox=x=%d:y=%d:w=%d:h=%d:color=black@0.35:t=fill[b1]", last, bx, by, boxW, boxH),
			fmt.Sprintf("[b1][lg]overlay=x=%d:y=%d[b2]", bx+pad, by+pad),
			fmt.Sprintf("[b2]drawtext=fontfile=%s:textfile=%s:fontsize=%d:fontcolor=white@0.95:x=%d:y=%d+(%d-text_h)/2[wm]",
				w.fontFile, textFile, fs, bx+pad+lw+gap, by+pad, lh),
		)
		last = "wm"
	}
	return finishGraph(g, last, thumbW, thumbH)
}

func plainGraph(outW, outH, thumbW, thumbH int) string {
	return finishGraph([]string{fmt.Sprintf("[0:v]scale=%d:%d:flags=lanczos[b0]", outW, outH)}, "b0", thumbW, thumbH)
}

func finishGraph(g []string, last string, thumbW, thumbH int) string {
	if thumbW > 0 {
		g = append(g,
			fmt.Sprintf("[%s]split=2[main][t0]", last),
			fmt.Sprintf("[t0]scale=%d:%d:flags=area[thumb]", thumbW, thumbH))
	} else {
		g = append(g, fmt.Sprintf("[%s]null[main]", last))
	}
	return strings.Join(g, ";")
}

type convertJob struct {
	src, tmpDir    string
	mainOut        string // final-name .part paths
	thumbOut       string // empty = no thumbnail
	capturedAt     time.Time
	watermark      bool
	outW, outH     int
	thumbW, thumbH int
	webp           WebPOptions
	thumbQ         int
	wm             *watermark
}

// convert decodes the source once and writes the watermarked WebP and thumbnail in
// a single ffmpeg pass, with bounded threads to keep memory low.
func (j *convertJob) convert(ctx context.Context) error {
	var graph string
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-threads", "2", "-filter_threads", "1", "-i", j.src}
	if j.watermark {
		textFile := filepath.Join(j.tmpDir, "stamp.txt")
		stamp := j.capturedAt.Format("2006-01-02 15:04 MST")
		if err := os.WriteFile(textFile, []byte(stamp), 0o600); err != nil {
			return err
		}
		args = append(args, "-i", j.wm.logoFile)
		graph = j.wm.filterGraph(j.outW, j.outH, stamp, textFile, j.thumbW, j.thumbH)
	} else {
		graph = plainGraph(j.outW, j.outH, j.thumbW, j.thumbH)
	}
	args = append(args, "-filter_complex", graph)

	preset := j.webp.Preset
	if preset == "" {
		preset = "default"
	}
	ll := "0"
	if j.webp.Lossless {
		ll = "1"
	}
	args = append(args, "-map", "[main]", "-frames:v", "1", "-c:v", "libwebp",
		"-quality", strconv.Itoa(j.webp.Quality), "-compression_level", strconv.Itoa(j.webp.CompressionLevel),
		"-lossless", ll, "-preset", preset, "-f", "webp", "-y", j.mainOut)
	if j.thumbOut != "" {
		args = append(args, "-map", "[thumb]", "-frames:v", "1", "-c:v", "libwebp",
			"-quality", strconv.Itoa(j.thumbQ), "-compression_level", strconv.Itoa(j.webp.CompressionLevel),
			"-lossless", "0", "-f", "webp", "-y", j.thumbOut)
	}
	out, err := run(ctx, nil, "ffmpeg", args...)
	if err != nil {
		if errors.Is(err, errTimeout) {
			return errf(504, "convert_timeout", "image conversion timed out")
		}
		var ae *apiError
		if errors.As(err, &ae) {
			return err
		}
		return errf(500, "convert_failed", "WebP conversion failed: %s", lastLine(out))
	}
	return nil
}
