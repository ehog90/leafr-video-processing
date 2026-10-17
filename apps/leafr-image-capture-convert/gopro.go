package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"time"
)

var (
	goproFolderRe = regexp.MustCompile(`^[0-9A-Za-z_]{1,32}$`)
	goproFileRe   = regexp.MustCompile(`^[0-9A-Za-z_]{1,32}\.(?i:jpg|jpeg)$`)
)

const goproMaxDownload = 100 << 20

type goproClient struct {
	base string
	http *http.Client
}

func (g *goproClient) get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.base+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, errTimeout
		}
		return nil, errf(502, "gopro_unreachable", "cannot reach GoPro at %s (powered on, connected, right IP?)", g.base[len("http://"):])
	}
	return resp, nil
}

// getJSON GETs a control endpoint; a 404 yields (nil, nil) when allow404 is set.
func (g *goproClient) getJSON(ctx context.Context, path string, allow404 bool, out any) error {
	resp, err := g.get(ctx, path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound && allow404 {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return errf(502, "gopro_error", "GoPro answered %d for %s", resp.StatusCode, path)
	}
	if out == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return errf(502, "gopro_error", "unexpected GoPro response from %s", path)
	}
	return nil
}

type goproMedia struct {
	Folder string `json:"folder"`
	File   string `json:"file"`
}

func (g *goproClient) lastCaptured(ctx context.Context) (goproMedia, error) {
	var m goproMedia
	err := g.getJSON(ctx, "/gopro/media/last_captured", true, &m)
	return m, err
}

// captureGoPro takes a photo via the Open GoPro HTTP API and streams the JPEG into dst.
func (c *Config) captureGoPro(ctx context.Context, s Source, dst string) error {
	g := &goproClient{
		base: "http://" + s.goproAddr(),
		http: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	if s.SetPhotoMode == nil || *s.SetPhotoMode {
		if err := g.getJSON(ctx, "/gopro/camera/presets/set_group?id=1001", false, nil); err != nil {
			return err
		}
	}
	before, err := g.lastCaptured(ctx)
	if err != nil {
		return err
	}
	if err := g.getJSON(ctx, "/gopro/camera/shutter/start", false, nil); err != nil {
		return err
	}

	var media goproMedia
	for tick := time.NewTicker(500 * time.Millisecond); ; {
		select {
		case <-ctx.Done():
			tick.Stop()
			return errTimeout
		case <-tick.C:
		}
		if media, err = g.lastCaptured(ctx); err != nil {
			tick.Stop()
			return err
		}
		if media != (goproMedia{}) && media != before {
			tick.Stop()
			break
		}
	}
	if !goproFolderRe.MatchString(media.Folder) || !goproFileRe.MatchString(media.File) {
		return errf(502, "gopro_unsupported_media", "GoPro captured %q/%q, expected a JPEG photo (is the camera in photo mode?)", media.Folder, media.File)
	}

	if err := g.download(ctx, "/videos/DCIM/"+media.Folder+"/"+media.File, dst); err != nil {
		return err
	}
	if s.DeleteAfterCapture {
		// Best effort: the image is already safe on our side.
		g.getJSON(ctx, "/gopro/media/delete/file?path="+media.Folder+"/"+media.File, false, nil)
	}
	return nil
}

func (g *goproClient) download(ctx context.Context, path, dst string) error {
	resp, err := g.get(ctx, path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errf(502, "gopro_download_failed", "GoPro answered %d while downloading the photo", resp.StatusCode)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(f, io.LimitReader(resp.Body, goproMaxDownload+1))
	switch {
	case err != nil && ctx.Err() != nil:
		return errTimeout
	case err != nil:
		return errf(502, "gopro_download_failed", "photo download interrupted: %v", err)
	case n > goproMaxDownload:
		return errf(502, "gopro_download_failed", "photo exceeds %d MB", goproMaxDownload>>20)
	case n == 0:
		return errors.New("gopro returned an empty photo")
	}
	return nil
}

var _ = fmt.Sprintf
