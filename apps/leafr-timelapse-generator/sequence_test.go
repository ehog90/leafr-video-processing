package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testMedia(t *testing.T) *mediaDir {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m, err := newMediaDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func touch(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeSeq(t *testing.T, m *mediaDir, body string) string {
	t.Helper()
	p := filepath.Join(m.root, "seq.imageseq")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadSequenceCarriesMissingDurations(t *testing.T) {
	m := testMedia(t)
	a, c := filepath.Join(m.root, "a.webp"), filepath.Join(m.root, "c.webp")
	touch(t, a)
	touch(t, c)
	body := "ffconcat version 1.0\n" +
		"file '" + a + "'\nduration 0.5\n" +
		"file 'missing1.webp'\nduration 0.25\n" + // relative to the media dir
		"file '" + filepath.Join(m.root, "missing2.webp") + "'\nduration 0.25\n" +
		"file c.webp\nduration 1\n" +
		"file '" + filepath.Join(m.root, "gone.webp") + "'\nduration 2\n" + // trailing
		"file c.webp\n" // repeated last file without a duration
	seq, err := loadSequence(context.Background(), writeSeq(t, m, body), m)
	if err != nil {
		t.Fatal(err)
	}
	if len(seq.Frames) != 2 || seq.Missing != 3 {
		t.Fatalf("frames=%d missing=%d", len(seq.Frames), seq.Missing)
	}
	if seq.Frames[0].Duration != 0.5 || seq.Frames[1].Duration != 3.5 {
		t.Fatalf("durations %v", seq.Frames)
	}
	if seq.Total != 4.0 {
		t.Fatalf("total %v", seq.Total)
	}
}

func TestLoadSequenceRejects(t *testing.T) {
	m := testMedia(t)
	cases := map[string]string{
		"outside":       "file '" + filepath.Join(filepath.Dir(m.root), "x.webp") + "'\nduration 1\n",
		"traversal":     "file '../x.webp'\nduration 1\n",
		"directive":     "ffconcat version 1.0\noption protocol_whitelist file\n",
		"url":           "file 'http://example.com/x'\nduration 1\n", // relative -> inside media, but unsupported scheme is just a missing file
		"bad duration":  "file a.webp\nduration abc\n",
		"neg duration":  "file a.webp\nduration -1\n",
		"orphan":        "duration 1\n",
		"open quote":    "file 'a.webp\nduration 1\n",
		"missing path":  "file\nduration 1\n",
		"inf duration":  "file a.webp\nduration Inf\n",
		"nan duration":  "file a.webp\nduration NaN\n",
		"outside quote": "file 'a'\\''/../../x'\nduration 1\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			seq, err := loadSequence(context.Background(), writeSeq(t, m, body), m)
			if name == "url" {
				if err != nil || len(seq.Frames) != 0 {
					t.Fatalf("url entry should only count as missing, got %v %v", seq, err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestLoadSequenceMissingFile(t *testing.T) {
	m := testMedia(t)
	_, err := loadSequence(context.Background(), filepath.Join(m.root, "nope"), m)
	var jf *jobFailure
	if err == nil || !asJobFailure(err, &jf) || jf.Code != "sequence_not_found" {
		t.Fatalf("got %v", err)
	}
}

func asJobFailure(err error, target **jobFailure) bool {
	jf, ok := err.(*jobFailure)
	if ok {
		*target = jf
	}
	return ok
}

func TestLoadSequenceSymlinkEscape(t *testing.T) {
	m := testMedia(t)
	outside := t.TempDir()
	touch(t, filepath.Join(outside, "secret.webp"))
	if err := os.Symlink(outside, filepath.Join(m.root, "link")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	_, err := loadSequence(context.Background(), writeSeq(t, m, "file link/secret.webp\nduration 1\n"), m)
	if err == nil {
		t.Fatal("expected symlink escape to be rejected")
	}
}

func TestWriteConcatList(t *testing.T) {
	seq := &sequence{Frames: []frame{{"/media/it's.webp", 0.5}, {"/media/b.webp", 1.25}}}
	p, err := writeConcatList(t.TempDir(), seq)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	want := "ffconcat version 1.0\n" +
		"file '/media/it'\\''s.webp'\nduration 0.500000\n" +
		"file '/media/b.webp'\nduration 1.250000\n" +
		"file '/media/b.webp'\n"
	if string(b) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b, want)
	}
	// What we write must be readable by our own parser.
	tok, _ := parseToken(strings.TrimPrefix(strings.Split(string(b), "\n")[1], "file "))
	if tok != "/media/it's.webp" {
		t.Fatalf("round trip: %q", tok)
	}
}
