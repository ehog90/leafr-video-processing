# leafr-image-capture-convert

Go HTTP service: captures one frame from a local V4L2 camera (`fswebcam`) or an RTSP stream (`ffmpeg`, user/password), converts it to WebP (+ thumbnail) and returns the saved paths.

## Run

```
docker compose up --build        # set VIDEO_GID to the host `video` group id if not 44
```

Devices are mapped read-only from the host, one by one, in `docker-compose.yml`; the app runs as a non-root user, read-only rootfs, no capabilities. `ALLOWED_DEVICES` (comma list) restricts which `/dev/videoN` can be requested.

## API

`POST /capture`

```json
{
  "source": { "type": "local", "device": "/dev/video0" },
  "webp": { "width": 1280, "height": 0, "quality": 80, "lossless": false, "compression_level": 4, "preset": "photo" },
  "thumbnail": { "enabled": true, "width": 320, "height": 0, "quality": 70 },
  "subdir": "cam1", "name": "garden"
}
```

RTSP source: `{"type":"rtsp","url":"rtsp://host:554/stream","username":"u","password":"p"}` (credentials separate from the URL, redacted from errors).

Response: `{"path":"/data/images/cam1/garden_..._ab12cd34.webp","thumbnail_path":"...thumb.webp","source":"local","captured_resolution":"1920x1080"}`

Width/height: `0` = derived from the other / original; both set = fit inside, aspect kept. `GET /healthz` for health.

Local capture: fswebcam, no banner/overlay, maximal resolution (from `v4l2-ctl --list-formats-ext`, or `source.resolution`). The intermediate is a lossless PNG (fswebcam's JPEG cannot reach true 100%), so no quality is lost before WebP.

## Environment defaults

`LISTEN_ADDR`, `OUTPUT_DIR`, `ALLOWED_DEVICES`, `MAX_CONCURRENT`, `CAPTURE_TIMEOUT_SECONDS`, `FSWEBCAM_SKIP_FRAMES`, `WEBP_{WIDTH,HEIGHT,QUALITY,COMPRESSION_LEVEL,PRESET}`, `THUMB_{WIDTH,HEIGHT,QUALITY}`.
