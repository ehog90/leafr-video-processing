package main

import (
	"context"
	"os"
	"path/filepath"
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
//  1. preparing: read the sequence file, drop missing pictures, write the list ffmpeg reads
//  2. encoding: ffmpeg concat -> video (progress from ffmpeg's -progress stream)
//  3. thumbnail (optional): ffmpeg video -> animated thumbnail
//  4. publish: rename the partial files to their final names
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
		os.Remove(videoTmp)
		if thumbTmp != "" {
			os.Remove(thumbTmp)
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
	err = runFFmpeg(ctx, p.cfg.FFmpegBin, encodeArgs(p.cfg, spec, listPath, videoTmp, seq.Total), func(sec float64) {
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
	if err := p.publish(spec, videoTmp, thumbTmp, res); err != nil {
		return nil, err
	}

	if spec.DeleteSequenceFile {
		res.SequenceFileDeleted = os.Remove(spec.SequenceFile) == nil
	}
	return res, nil
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
