package main

import (
	"bufio"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func fmtSec(v float64) string { return strconv.FormatFloat(v, 'f', 6, 64) }

func containerFor(outputFile string) string {
	if strings.EqualFold(filepath.Ext(outputFile), ".mkv") {
		return "matroska"
	}
	return "mp4"
}

func quietInput() []string {
	return []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-nostats", "-progress", "pipe:1"}
}

// encodeArgs builds the ffmpeg arguments that turn the concat list into the video.
//
// The picture is scaled to fit within width x height keeping the aspect ratio (no padding;
// dimensions are rounded down to even numbers as yuv420p requires). -t pins the output to
// the total sequence length so the repeated last picture adds nothing.
func encodeArgs(cfg *Config, spec *Spec, listPath, outPath string, totalSec float64) []string {
	args := quietInput()
	args = append(args,
		"-f", "concat", "-safe", "0", "-i", listPath,
		"-an",
		"-vf", "scale=w="+strconv.Itoa(spec.Width)+":h="+strconv.Itoa(spec.Height)+
			":force_original_aspect_ratio=decrease:force_divisible_by=2",
		"-pix_fmt", "yuv420p",
		"-r", strconv.Itoa(spec.FPS),
		"-t", fmtSec(totalSec),
		"-c:v", codecEncoders[spec.Codec],
	)
	switch spec.Codec {
	case "AV1":
		args = append(args,
			"-preset", strconv.Itoa(cfg.AV1Preset),
			"-crf", strconv.Itoa(cfg.AV1CRF),
			"-svtav1-params", "lp="+strconv.Itoa(cfg.Threads))
	case "HEVC":
		args = append(args, "-tag:v", "hvc1", "-x265-params", "log-level=error", "-threads", strconv.Itoa(cfg.Threads))
	default:
		args = append(args, "-threads", strconv.Itoa(cfg.Threads))
	}
	container := containerFor(spec.OutputFile)
	if container == "mp4" {
		args = append(args, "-movflags", "+faststart")
	}
	return append(args, "-f", container, "-y", outPath)
}

// thumbArgs builds the ffmpeg arguments for the animated thumbnail of the finished video.
// Videos longer than ThumbMaxSec are sped up to that length. It also returns the expected
// thumbnail length in seconds, which is the progress denominator.
func thumbArgs(cfg *Config, t *ThumbSpec, inPath, outPath string, videoSec float64) ([]string, float64) {
	thumbSec := videoSec
	filters := make([]string, 0, 3)
	if videoSec > cfg.ThumbMaxSec {
		thumbSec = cfg.ThumbMaxSec
		filters = append(filters, "setpts=PTS*"+strconv.FormatFloat(cfg.ThumbMaxSec/videoSec, 'f', 9, 64))
	}
	filters = append(filters,
		"fps="+strconv.Itoa(cfg.ThumbFPS),
		"scale='min("+strconv.Itoa(t.Width)+",iw)':-2:flags=lanczos")

	args := quietInput()
	args = append(args, "-i", inPath, "-an", "-vf", strings.Join(filters, ","))
	if t.Format == "avif" {
		args = append(args, "-c:v", "libsvtav1", "-pix_fmt", "yuv420p",
			"-crf", strconv.Itoa(cfg.ThumbAVIFCRF), "-preset", "8", "-f", "avif")
	} else {
		args = append(args, "-c:v", "libwebp", "-quality", strconv.Itoa(cfg.ThumbWebPQuality),
			"-compression_level", "4", "-loop", "0", "-f", "webp")
	}
	return append(args, "-y", outPath), thumbSec
}

// runFFmpeg runs ffmpeg to completion, reporting the encoded position in seconds read from
// its -progress stream. The process is killed when ctx is cancelled and ctx.Err() is
// returned in that case.
func runFFmpeg(ctx context.Context, bin string, args []string, onProgress func(sec float64)) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = 3 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return failf("encode_failed", "ffmpeg: %v", err)
	}
	var stderr tailBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return failf("ffmpeg_unavailable", "cannot start %s: %v", bin, err)
	}

	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "out_time_us="); ok {
			if us, err := strconv.ParseInt(v, 10, 64); err == nil && us >= 0 {
				onProgress(float64(us) / 1e6)
			}
		}
	}
	err = cmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		var ee *exec.ExitError
		if errors.As(err, &ee) && msg != "" {
			return failf("encode_failed", "ffmpeg exited with %d: %s", ee.ExitCode(), msg)
		}
		return failf("encode_failed", "ffmpeg: %v", err)
	}
	return nil
}

// tailBuffer keeps the last bytes written to it; ffmpeg errors are at the end of stderr.
type tailBuffer struct{ b []byte }

const tailSize = 2048

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > tailSize {
		t.b = t.b[len(t.b)-tailSize:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }
