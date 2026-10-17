package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// mediaDir confines every file the service reads or writes to MEDIA_DIR.
type mediaDir struct {
	root     string // cleaned absolute path
	realRoot string // root with symlinks resolved
}

func newMediaDir(dir string) (*mediaDir, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(real); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	return &mediaDir{root: root, realRoot: real}, nil
}

// resolve turns p into a cleaned absolute path inside the media directory. Relative paths
// are taken relative to the media directory. It is purely lexical; use checkReal to also
// rule out symlinks that lead outside.
func (m *mediaDir) resolve(p string) (string, error) {
	if p == "" || strings.ContainsRune(p, 0) {
		return "", errors.New("invalid path")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(m.root, p)
	}
	p = filepath.Clean(p)
	if !isInside(m.root, p) {
		return "", errors.New("path is outside the media directory")
	}
	return p, nil
}

// checkReal verifies that p, or its nearest existing ancestor, does not escape the media
// directory through a symlink.
func (m *mediaDir) checkReal(p string) error {
	cur := p
	for {
		real, err := filepath.EvalSymlinks(cur)
		if err == nil {
			if real != m.realRoot && !isInside(m.realRoot, real) {
				return errors.New("path is outside the media directory")
			}
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return err
		}
		cur = parent
	}
}

// relative returns p relative to the media directory, with forward slashes.
func (m *mediaDir) relative(p string) string {
	rel, err := filepath.Rel(m.root, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(rel)
}

func isInside(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
