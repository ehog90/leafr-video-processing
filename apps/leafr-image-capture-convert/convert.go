package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
)

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

// wmFontSize is the watermark text size in output pixels. It is fixed rather than
// derived from the image size, so the watermark keeps the same size on every capture.
const wmFontSize = 16

type watermark struct {
	face       font.Face
	fontFile   string
	logo       image.Image
	logoAspect float64
}

func decodeImageFile(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	return img, err
}

func loadWatermark(c *Config) (*watermark, error) {
	data, err := os.ReadFile(c.FontFile)
	if err != nil {
		return nil, fmt.Errorf("font: %w", err)
	}
	logo, err := decodeImageFile(c.LogoFile)
	if err != nil {
		return nil, fmt.Errorf("logo: %w", err)
	}
	lb := logo.Bounds()
	if lb.Dx() <= 0 || lb.Dy() <= 0 {
		return nil, fmt.Errorf("logo: invalid dimensions %dx%d", lb.Dx(), lb.Dy())
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
	return &watermark{face: face, fontFile: c.FontFile, logo: logo, logoAspect: float64(lb.Dx()) / float64(lb.Dy())}, nil
}

func (w *watermark) textWidth(text string, fontSize int) int {
	adv := font.MeasureString(w.face, text)
	return int(math.Ceil(float64(adv.Ceil()) * float64(fontSize) / 100))
}

// panelOpacity is the opacity of the black box behind the watermark.
const panelOpacity = 0.35

// wmLayout is the watermark geometry in output pixels. The panel sits in the
// bottom-left corner; the logo and timestamp are placed inside it.
type wmLayout struct {
	fs, lw, lh, gap, pad int
	bx, by, boxW, boxH   int
	radius               int
}

// layout computes the watermark geometry. ok is false when the panel does not fit the image.
func (w *watermark) layout(outW, outH int, stamp string) (l wmLayout, ok bool) {
	l.fs = wmFontSize
	l.lh = int(math.Round(float64(l.fs) * 1.7))
	l.lw = int(math.Round(float64(l.lh) * w.logoAspect))
	l.gap = int(math.Round(float64(l.fs) * 0.5))
	l.pad = l.fs
	margin := int(math.Round(float64(l.fs) * 0.9))
	l.boxW = l.lw + l.gap + w.textWidth(stamp, l.fs) + 2*l.pad
	l.boxH = l.lh + 2*l.pad
	l.bx, l.by = margin, outH-margin-l.boxH
	l.radius = min(int(math.Round(float64(l.fs)*0.7)), l.boxH/2)
	return l, l.bx+l.boxW <= outW && l.by >= 0
}

// filterGraph builds the ffmpeg -filter_complex graph. Inputs: 0 = source image,
// 1 = logo at its final size (see writeScaledLogo), 2 = rounded panel (see
// writeRoundedPanel). Output pad [main] is the watermarked full image; [thumb] is
// added when thumbnail dimensions are given.
func (w *watermark) filterGraph(outW, outH int, l wmLayout, textFile string, thumbW, thumbH int) string {
	g := []string{fmt.Sprintf("[0:v]scale=%d:%d:flags=lanczos[b0]", outW, outH)}
	// The thumbnail is split off before the watermark so it never carries it.
	g, src := splitThumbnail(g, "b0", thumbW, thumbH)
	g = append(g,
		fmt.Sprintf("[%s][2:v]overlay=x=%d:y=%d[b1]", src, l.bx, l.by),
		fmt.Sprintf("[b1][1:v]overlay=x=%d:y=%d[b2]", l.bx+l.pad, l.by+l.pad),
		fmt.Sprintf("[b2]drawtext=fontfile=%s:textfile=%s:fontsize=%d:fontcolor=white@0.95:x=%d:y=%d+(%d-text_h)/2[wm]",
			w.fontFile, textFile, l.fs, l.bx+l.pad+l.lw+l.gap, l.by+l.pad, l.lh),
	)
	return finishGraph(g, "wm")
}

// writeRoundedPanel writes a black rounded rectangle at the given opacity as an RGBA PNG.
// ffmpeg's drawbox cannot round corners, so the panel is rendered here. Edges are
// anti-aliased with 4x4 supersampling.
func writeRoundedPanel(path string, w, h, r int, opacity float64) error {
	const ss = 4
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	maxA := opacity * 255
	fw, fh, fr := float64(w), float64(h), float64(r)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			hits := 0
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					px := float64(x) + (float64(sx)+0.5)/ss
					py := float64(y) + (float64(sy)+0.5)/ss
					if insideRoundedRect(px, py, fw, fh, fr) {
						hits++
					}
				}
			}
			img.SetNRGBA(x, y, color.NRGBA{A: uint8(math.Round(maxA * float64(hits) / (ss * ss)))})
		}
	}
	return writePNG(path, img)
}

// writeScaledLogo resamples the logo to w×h and writes it as PNG. This is done here
// instead of by ffmpeg's scale filter, which does not account for alpha and can leave
// dark fringes around transparent edges. The Catmull-Rom kernel also low-pass filters
// when shrinking, so the result is antialiased rather than aliased.
func writeScaledLogo(path string, logo image.Image, w, h int) error {
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), logo, logo.Bounds(), xdraw.Src, nil)
	return writePNG(path, dst)
}

func writePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// insideRoundedRect reports whether (x, y) lies inside a w×h rectangle with corner radius r.
func insideRoundedRect(x, y, w, h, r float64) bool {
	cx := math.Max(r, math.Min(x, w-r))
	cy := math.Max(r, math.Min(y, h-r))
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= r*r
}

func plainGraph(outW, outH, thumbW, thumbH int) string {
	g := []string{fmt.Sprintf("[0:v]scale=%d:%d:flags=lanczos[b0]", outW, outH)}
	g, src := splitThumbnail(g, "b0", thumbW, thumbH)
	return finishGraph(g, src)
}

// splitThumbnail derives [thumb] from base when thumbnail dimensions are given and
// returns the label the main output should be taken from.
func splitThumbnail(g []string, base string, thumbW, thumbH int) ([]string, string) {
	if thumbW <= 0 {
		return g, base
	}
	g = append(g,
		fmt.Sprintf("[%s]split=2[mainsrc][t0]", base),
		fmt.Sprintf("[t0]scale=%d:%d:flags=area[thumb]", thumbW, thumbH))
	return g, "mainsrc"
}

func finishGraph(g []string, last string) string {
	g = append(g, fmt.Sprintf("[%s]null[main]", last))
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
	var wmLay wmLayout
	useWM := false
	if j.watermark {
		stamp := j.capturedAt.Format("2006-01-02 15:04 MST")
		var fits bool
		wmLay, fits = j.wm.layout(j.outW, j.outH, stamp)
		useWM = fits
	}
	if useWM {
		textFile := filepath.Join(j.tmpDir, "stamp.txt")
		panelFile := filepath.Join(j.tmpDir, "panel.png")
		stamp := j.capturedAt.Format("2006-01-02 15:04 MST")
		if err := os.WriteFile(textFile, []byte(stamp), 0o600); err != nil {
			return err
		}
		if err := writeRoundedPanel(panelFile, wmLay.boxW, wmLay.boxH, wmLay.radius, panelOpacity); err != nil {
			return err
		}
		logoFile := filepath.Join(j.tmpDir, "logo.png")
		if err := writeScaledLogo(logoFile, j.wm.logo, wmLay.lw, wmLay.lh); err != nil {
			return err
		}
		args = append(args, "-i", logoFile, "-i", panelFile)
		graph = j.wm.filterGraph(j.outW, j.outH, wmLay, textFile, j.thumbW, j.thumbH)
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
		if strings.Contains(strings.ToLower(out), "permission denied") {
			return errf(500, "output_permission_denied", "output directory is not writable by the service user (check host ownership of OUTPUT_DIR)")
		}
		return errf(500, "convert_failed", "WebP conversion failed: %s", lastLine(out))
	}
	return nil
}
