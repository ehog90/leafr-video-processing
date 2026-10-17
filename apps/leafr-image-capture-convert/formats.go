package main

import "strconv"

// supportedFormats lists the output formats the service can encode. Adding a format
// means: an entry here, a case in outputExt and encoderArgs, and an options block on
// ImageOptions/ThumbnailOptions when it needs settings of its own.
var supportedFormats = map[string]string{
	"webp": "webp",
}

// outputExt returns the file extension for a supported format.
func outputExt(format string) string {
	return supportedFormats[format]
}

// encoderArgs returns the ffmpeg codec and muxer arguments for one output.
func encoderArgs(s OutputSettings) []string {
	switch s.Format {
	case "webp":
		w := s.WebP
		preset := w.Preset
		if preset == "" {
			preset = "default"
		}
		lossless := "0"
		if w.Lossless {
			lossless = "1"
		}
		return []string{"-c:v", "libwebp",
			"-quality", strconv.Itoa(w.Quality),
			"-compression_level", strconv.Itoa(w.CompressionLevel),
			"-lossless", lossless, "-preset", preset, "-f", "webp"}
	}
	return nil
}
