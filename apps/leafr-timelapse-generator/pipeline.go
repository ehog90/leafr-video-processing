package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// pipeline turns one image sequence into a video and, optionally, an animated thumbnail.
type pipeline struct {
	cfg   *Config
	media *mediaDir
}

// encodeShare is the part of the overall progress that belongs to the video encode when a
// thumbnail follows it.
const encodeShare = 90.0

// run executes a job:
//
//  1. reading (set by the manager): read the sequence file and check the pictures
//  2. preparing: create the output folders and write the list ffmpeg reads
//  3. encoding: ffmpeg concat -> video (progress from ffmpeg's -progress stream)
//  4. thumbnail (optional): ffmpeg video -> animated thumbnail
//  5. saving: rename the partial files to their final names, delete the sequence file if asked
//
// Output is written to "<name>.partial" files and only renamed once everything succeeded,
// so a cancelled or failed run never leaves a truncated video behind.
func (p *pipeline) run(ctx context.Context, c *jobCtl) (*Result, error) {
	spec := c.Spec

	seq, err := loadSequence(ctx, spec.SequenceFile, p.media)
	if err != nil {
		return nil, err
	}
	if len(seq.Frames) < 2 {
		return nil, failf("not_enough_frames", "at least 2 pictures are needed, %d available (%d missing)", len(seq.Frames), seq.Missing)
	}
	c.Logf("📄 sequence read: %d pictures, %.1fs of video", len(seq.Frames), seq.Total)
	if seq.Missing > 0 {
		c.Logf("⚠️ %d pictures in the sequence no longer exist; their time was added to the neighbouring pictures", seq.Missing)
	}

	c.SetStatus(StatusPreparing, true)
	outputs := []string{spec.OutputFile}
	if spec.Thumbnail != nil {
		outputs = append(outputs, spec.Thumbnail.OutputFile)
	}
	for _, o := range outputs {
		dir := filepath.Dir(o)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, failf("output_unwritable", "cannot create %s: %v", dir, err)
		}
		if err := p.media.checkReal(dir); err != nil {
			return nil, failf("invalid_path", "%s: %v", o, err)
		}
	}

	videoTmp := spec.OutputFile + ".partial"
	thumbTmp := ""
	if spec.Thumbnail != nil {
		thumbTmp = spec.Thumbnail.OutputFile + ".partial"
	}
	defer func() {
		removed := removeIfExists(videoTmp)
		if thumbTmp != "" {
			removed = removeIfExists(thumbTmp) || removed
		}
		if removed {
			c.Logf("🧹 removed partial output files")
		}
	}()

	listPath, err := writeConcatList(p.cfg.TmpDir, seq)
	if err != nil {
		return nil, failf("internal_error", "cannot write the ffmpeg list: %v", err)
	}
	defer os.Remove(listPath)

	share := 100.0
	if spec.Thumbnail != nil {
		share = encodeShare
	}
	c.SetStatus(StatusEncoding, false)
	args := encodeArgs(p.cfg, spec, listPath, videoTmp, seq.Total)
	c.Logf("🎞️ ffmpeg %s", strings.Join(args, " "))
	err = runFFmpeg(ctx, p.cfg.FFmpegBin, args, func(sec float64) {
		c.SetProgress(sec / seq.Total * share)
	})
	if err != nil {
		return nil, err
	}

	res := &Result{
		DurationSec:   seq.Total,
		FramesUsed:    len(seq.Frames),
		MissingFrames: seq.Missing,
	}

	if t := spec.Thumbnail; t != nil {
		c.SetStatus(StatusThumbnail, false)
		args, thumbSec := thumbArgs(p.cfg, t, videoTmp, thumbTmp, seq.Total)
		c.Logf("🎞️ ffmpeg %s", strings.Join(args, " "))
		res.ThumbnailDurationSec = thumbSec
		err = runFFmpeg(ctx, p.cfg.FFmpegBin, args, func(sec float64) {
			c.SetProgress(encodeShare + sec/thumbSec*(100-encodeShare))
		})
		if err != nil {
			return nil, err
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.SetStatus(StatusSaving, true)
	if err := p.publish(spec, videoTmp, thumbTmp, res); err != nil {
		return nil, err
	}
	c.Logf("📦 output written: %s (%s)", res.OutputFile, humanSize(res.SizeBytes))
	if res.ThumbnailFile != "" {
		c.Logf("📦 thumbnail written: %s (%s)", res.ThumbnailFile, humanSize(res.ThumbnailSizeBytes))
	}

	if spec.DeleteSequenceFile {
		if err := os.Remove(spec.SequenceFile); err != nil {
			c.Logf("⚠️ could not delete the sequence file: %v", err)
		} else {
			res.SequenceFileDeleted = true
			c.Logf("🗑️ sequence file deleted")
		}
	}
	return res, nil
}

// removeIfExists deletes path and reports whether there was something to delete.
func removeIfExists(path string) bool {
	return os.Remove(path) == nil
}

// publish moves the finished partial files to their final names and fills in the result.
func (p *pipeline) publish(spec *Spec, videoTmp, thumbTmp string, res *Result) error {
	if !spec.Overwrite {
		for _, o := range []string{spec.OutputFile, thumbPath(spec)} {
			if o == "" {
				continue
			}
			if _, err := os.Lstat(o); err == nil {
				return failf("output_exists", "%s already exists", o)
			}
		}
	}
	if err := os.Rename(videoTmp, spec.OutputFile); err != nil {
		return failf("output_unwritable", "cannot write %s: %v", spec.OutputFile, err)
	}
	if spec.Thumbnail != nil {
		if err := os.Rename(thumbTmp, spec.Thumbnail.OutputFile); err != nil {
			os.Remove(spec.OutputFile)
			return failf("output_unwritable", "cannot write %s: %v", spec.Thumbnail.OutputFile, err)
		}
	}

	res.OutputFile = spec.OutputFile
	res.RelativeOutputFile = p.media.relative(spec.OutputFile)
	if st, err := os.Stat(spec.OutputFile); err == nil {
		res.SizeBytes = st.Size()
	}
	if spec.Thumbnail != nil {
		res.ThumbnailFile = spec.Thumbnail.OutputFile
		res.RelativeThumbnailFile = p.media.relative(spec.Thumbnail.OutputFile)
		if st, err := os.Stat(spec.Thumbnail.OutputFile); err == nil {
			res.ThumbnailSizeBytes = st.Size()
		}
	}
	return nil
}

func thumbPath(spec *Spec) string {
	if spec.Thumbnail == nil {
		return ""
	}
	return spec.Thumbnail.OutputFile
}
