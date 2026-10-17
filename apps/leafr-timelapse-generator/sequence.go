package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type frame struct {
	Path     string
	Duration float64 // seconds the picture is on screen
}

// sequence is the usable content of an image sequence file.
type sequence struct {
	Frames  []frame
	Total   float64 // sum of the frame durations in seconds
	Missing int     // entries skipped because the picture file does not exist
}

type sequenceEntry struct {
	line     int
	path     string
	duration float64
}

// loadSequence reads the ffmpeg concat list written by the backend (ffconcat format:
// `file '<path>'` lines, each followed by `duration <seconds>`).
//
// Only these directives are accepted; anything else is rejected so the list cannot make
// ffmpeg read arbitrary inputs. Every picture path must stay inside the media directory.
// Pictures that no longer exist are skipped and their duration is added to the next
// available picture (or to the last one when the missing pictures are trailing), so the
// total timelapse length is preserved. Entries without a duration (such as a repeated last
// file, the usual concat trick) carry no time and are dropped.
func loadSequence(ctx context.Context, path string, media *mediaDir) (*sequence, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, failf("sequence_not_found", "sequence file does not exist")
		}
		return nil, failf("sequence_unreadable", "cannot open sequence file: %v", err)
	}
	defer f.Close()

	entries, err := parseSequence(f, media)
	if err != nil {
		return nil, err
	}

	seq := &sequence{}
	pending := 0.0
	for i, e := range entries {
		if e.duration <= 0 {
			continue
		}
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		ok, err := pictureAvailable(e.path, media)
		if err != nil {
			return nil, failf("invalid_sequence", "line %d: %v", e.line, err)
		}
		if !ok {
			seq.Missing++
			pending += e.duration
			continue
		}
		seq.Frames = append(seq.Frames, frame{Path: e.path, Duration: e.duration + pending})
		pending = 0
	}
	if pending > 0 && len(seq.Frames) > 0 {
		seq.Frames[len(seq.Frames)-1].Duration += pending
	}
	for _, fr := range seq.Frames {
		seq.Total += fr.Duration
	}
	return seq, nil
}

func pictureAvailable(p string, media *mediaDir) (bool, error) {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return false, nil
	}
	if !isInside(media.realRoot, real) {
		return false, errors.New("path is outside the media directory")
	}
	st, err := os.Stat(real)
	if err != nil || !st.Mode().IsRegular() {
		return false, nil
	}
	return true, nil
}

func parseSequence(f *os.File, media *mediaDir) ([]sequenceEntry, error) {
	var entries []sequenceEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if n == 1 {
			line = strings.TrimPrefix(line, "\ufeff")
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		directive, arg := line, ""
		if i := strings.IndexAny(line, " \t"); i >= 0 {
			directive, arg = line[:i], strings.TrimSpace(line[i+1:])
		}
		switch directive {
		case "ffconcat":
			// version header
		case "file":
			p, err := parseToken(arg)
			if err != nil || p == "" {
				return nil, failf("invalid_sequence", "line %d: invalid file path", n)
			}
			resolved, err := media.resolve(p)
			if err != nil {
				return nil, failf("invalid_sequence", "line %d: %v", n, err)
			}
			entries = append(entries, sequenceEntry{line: n, path: resolved})
		case "duration":
			if len(entries) == 0 {
				return nil, failf("invalid_sequence", "line %d: duration before any file", n)
			}
			tok, err := parseToken(arg)
			if err != nil {
				return nil, failf("invalid_sequence", "line %d: invalid duration", n)
			}
			d, err := strconv.ParseFloat(tok, 64)
			if err != nil || d < 0 || math.IsNaN(d) || math.IsInf(d, 0) {
				return nil, failf("invalid_sequence", "line %d: invalid duration %q", n, tok)
			}
			entries[len(entries)-1].duration = d
		default:
			return nil, failf("invalid_sequence", "line %d: unsupported directive %q", n, directive)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, failf("sequence_unreadable", "cannot read sequence file: %v", err)
	}
	return entries, nil
}

// parseToken reads one ffconcat argument: single-quoted parts, backslash escapes and plain
// characters, ending at the first unquoted whitespace.
func parseToken(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c == '\'':
			i++
			for i < len(s) && s[i] != '\'' {
				b.WriteByte(s[i])
				i++
			}
			if i >= len(s) {
				return "", errors.New("unterminated quote")
			}
			i++
		case c == '\\':
			i++
			if i < len(s) {
				b.WriteByte(s[i])
				i++
			}
		case c == ' ' || c == '\t':
			return b.String(), nil
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), nil
}

func quoteConcat(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// writeConcatList writes the sanitised list that ffmpeg actually reads. The last picture
// is repeated without a duration because the concat demuxer ignores the duration of the
// final entry otherwise.
func writeConcatList(dir string, seq *sequence) (string, error) {
	f, err := os.CreateTemp(dir, "timelapse_*.ffconcat")
	if err != nil {
		return "", err
	}
	w := bufio.NewWriter(f)
	fmt.Fprintln(w, "ffconcat version 1.0")
	for _, fr := range seq.Frames {
		fmt.Fprintf(w, "file %s\nduration %s\n", quoteConcat(fr.Path), strconv.FormatFloat(fr.Duration, 'f', 6, 64))
	}
	if n := len(seq.Frames); n > 0 {
		fmt.Fprintf(w, "file %s\n", quoteConcat(seq.Frames[n-1].Path))
	}
	if err := w.Flush(); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
