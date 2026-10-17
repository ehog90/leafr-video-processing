package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

func extOf(p string) string { return filepath.Ext(p) }

// Job statuses. A job moves queued -> preparing -> encoding -> [thumbnail] -> completed,
// or ends in failed / cancelled from any non-terminal state.
const (
	StatusQueued    = "queued"
	StatusPreparing = "preparing"
	StatusEncoding  = "encoding"
	StatusThumbnail = "thumbnail"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

func isTerminal(status string) bool {
	return status == StatusCompleted || status == StatusFailed || status == StatusCancelled
}

// Codecs maps the API codec name to the ffmpeg encoder.
var codecEncoders = map[string]string{
	"H264": "libx264",
	"HEVC": "libx265",
	"AV1":  "libsvtav1",
}

var allowedFPS = map[int]bool{24: true, 30: true, 60: true}

const (
	minDimension = 16
	maxDimension = 16384
	defaultFPS   = 30
)

// CreateRequest is the body of POST /api/timelapses.
type CreateRequest struct {
	// SequenceFile is an ffmpeg concat list (ffconcat) written by the backend.
	SequenceFile string `json:"sequenceFile"`
	OutputFile   string `json:"outputFile"`
	Codec        string `json:"codec"`
	FPS          int    `json:"fps"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	// Overwrite allows replacing existing output files.
	Overwrite bool `json:"overwrite"`
	// DeleteSequenceFile removes the sequence file once the job completed successfully.
	DeleteSequenceFile bool              `json:"deleteSequenceFile"`
	Thumbnail          *ThumbnailRequest `json:"thumbnail"`
}

// ThumbnailRequest asks for an animated thumbnail of the finished video.
type ThumbnailRequest struct {
	OutputFile string `json:"outputFile"`
	Format     string `json:"format"` // webp | avif; defaults to THUMB_FORMAT
	Width      int    `json:"width"`  // maximum width in px; defaults to THUMB_WIDTH
}

// Spec is a validated request with every path resolved to an absolute path inside MEDIA_DIR.
type Spec struct {
	SequenceFile       string     `json:"sequenceFile"`
	OutputFile         string     `json:"outputFile"`
	Codec              string     `json:"codec"`
	FPS                int        `json:"fps"`
	Width              int        `json:"width"`
	Height             int        `json:"height"`
	Overwrite          bool       `json:"overwrite"`
	DeleteSequenceFile bool       `json:"deleteSequenceFile"`
	Thumbnail          *ThumbSpec `json:"thumbnail,omitempty"`
}

type ThumbSpec struct {
	OutputFile string `json:"outputFile"`
	Format     string `json:"format"`
	Width      int    `json:"width"`
}

// Result describes the files a completed job produced.
type Result struct {
	OutputFile            string  `json:"outputFile"`
	RelativeOutputFile    string  `json:"relativeOutputFile"`
	SizeBytes             int64   `json:"sizeBytes"`
	DurationSec           float64 `json:"durationSec"`
	FramesUsed            int     `json:"framesUsed"`
	MissingFrames         int     `json:"missingFrames"`
	ThumbnailFile         string  `json:"thumbnailFile,omitempty"`
	RelativeThumbnailFile string  `json:"relativeThumbnailFile,omitempty"`
	ThumbnailSizeBytes    int64   `json:"thumbnailSizeBytes,omitempty"`
	ThumbnailDurationSec  float64 `json:"thumbnailDurationSec,omitempty"`
	SequenceFileDeleted   bool    `json:"sequenceFileDeleted,omitempty"`
}

// JobError is the failure reason of a failed job.
type JobError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Job is the public, JSON-serialisable state of a timelapse job. It is what the REST API
// returns and what the WebSocket pushes on every change.
type Job struct {
	ID                    string     `json:"id"`
	Status                string     `json:"status"`
	ProgressPercent       float64    `json:"progressPercent"`
	ProgressIndeterminate bool       `json:"progressIndeterminate"`
	Request               Spec       `json:"request"`
	Result                *Result    `json:"result,omitempty"`
	Error                 *JobError  `json:"error,omitempty"`
	CreatedAt             time.Time  `json:"createdAt"`
	StartedAt             *time.Time `json:"startedAt,omitempty"`
	FinishedAt            *time.Time `json:"finishedAt,omitempty"`
}

// apiError is returned to clients as {"error":{"code","message"}}.
type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string { return e.Code + ": " + e.Message }

func errf(status int, code, format string, a ...any) error {
	return &apiError{Status: status, Code: code, Message: fmt.Sprintf(format, a...)}
}

// jobFailure is an error raised by the pipeline that carries a stable error code.
type jobFailure struct {
	Code string
	Err  error
}

func (e *jobFailure) Error() string { return e.Err.Error() }
func (e *jobFailure) Unwrap() error { return e.Err }

func failf(code, format string, a ...any) error {
	return &jobFailure{Code: code, Err: fmt.Errorf(format, a...)}
}

func asJobError(err error) *JobError {
	var jf *jobFailure
	if errors.As(err, &jf) {
		return &JobError{Code: jf.Code, Message: jf.Error()}
	}
	return &JobError{Code: "internal_error", Message: err.Error()}
}

// validateRequest checks the request and resolves its paths. It does not touch the
// filesystem beyond path resolution.
func validateRequest(req *CreateRequest, media *mediaDir, cfg *Config) (*Spec, error) {
	req.Codec = strings.ToUpper(strings.TrimSpace(req.Codec))
	if _, ok := codecEncoders[req.Codec]; !ok {
		return nil, errf(400, "invalid_request", "codec must be one of H264, HEVC, AV1")
	}
	if req.FPS == 0 {
		req.FPS = defaultFPS
	}
	if !allowedFPS[req.FPS] {
		return nil, errf(400, "invalid_request", "fps must be 24, 30 or 60")
	}
	if req.Width < minDimension || req.Width > maxDimension || req.Height < minDimension || req.Height > maxDimension {
		return nil, errf(400, "invalid_request", "width and height must be between %d and %d", minDimension, maxDimension)
	}
	if req.SequenceFile == "" || req.OutputFile == "" {
		return nil, errf(400, "invalid_request", "sequenceFile and outputFile are required")
	}

	seq, err := media.resolve(req.SequenceFile)
	if err != nil {
		return nil, errf(400, "invalid_path", "sequenceFile: %v", err)
	}
	out, err := media.resolve(req.OutputFile)
	if err != nil {
		return nil, errf(400, "invalid_path", "outputFile: %v", err)
	}
	if ext := strings.ToLower(extOf(out)); ext != ".mp4" && ext != ".mkv" {
		return nil, errf(400, "invalid_request", "outputFile must end with .mp4 or .mkv")
	}

	spec := &Spec{
		SequenceFile: seq, OutputFile: out, Codec: req.Codec, FPS: req.FPS,
		Width: req.Width, Height: req.Height,
		Overwrite: req.Overwrite, DeleteSequenceFile: req.DeleteSequenceFile,
	}

	if t := req.Thumbnail; t != nil {
		if t.OutputFile == "" {
			return nil, errf(400, "invalid_request", "thumbnail.outputFile is required")
		}
		format := strings.ToLower(strings.TrimSpace(t.Format))
		if format == "" {
			format = cfg.ThumbFormat
		}
		if format != "webp" && format != "avif" {
			return nil, errf(400, "invalid_request", "thumbnail.format must be webp or avif")
		}
		width := t.Width
		if width == 0 {
			width = cfg.ThumbWidth
		}
		if width < minDimension || width > maxDimension {
			return nil, errf(400, "invalid_request", "thumbnail.width must be between %d and %d", minDimension, maxDimension)
		}
		tOut, err := media.resolve(t.OutputFile)
		if err != nil {
			return nil, errf(400, "invalid_path", "thumbnail.outputFile: %v", err)
		}
		if strings.ToLower(extOf(tOut)) != "."+format {
			return nil, errf(400, "invalid_request", "thumbnail.outputFile must end with .%s", format)
		}
		if tOut == out {
			return nil, errf(400, "invalid_request", "thumbnail.outputFile must differ from outputFile")
		}
		spec.Thumbnail = &ThumbSpec{OutputFile: tOut, Format: format, Width: width}
	}
	if seq == out || (spec.Thumbnail != nil && seq == spec.Thumbnail.OutputFile) {
		return nil, errf(400, "invalid_request", "sequenceFile must differ from the output files")
	}
	return spec, nil
}
