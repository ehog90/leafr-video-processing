# leafr-image-capture-convert

Go HTTP service: captures one frame from a local V4L2 camera (`fswebcam`), an RTSP stream (`ffmpeg`, user/password) or a GoPro (Open GoPro HTTP API), converts it to WebP (+ thumbnail), adds a Leafr watermark and returns the saved paths.

## Run

```
docker compose up --build        # set VIDEO_GID to the host `video` group id if not 44
```

Devices are mapped read-only from the host, one by one, in `docker-compose.yml`; the app runs as a non-root user, read-only rootfs, no capabilities. `ALLOWED_DEVICES` (comma list) restricts which `/dev/videoN` can be requested.

## API

| Method | Path       | Purpose                                  |
|--------|------------|------------------------------------------|
| POST   | `/capture` | Capture one image, convert, return paths |
| GET    | `/healthz` | Liveness (`200 OK`)                      |

All bodies are JSON. Unknown fields are rejected with `400 invalid_request`.

### Request: `POST /capture`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `source` | object | yes | — | Where the frame comes from. See per-source fields below. |
| `webp` | object | no | see below | WebP output settings. |
| `thumbnail` | object | no | see below | Thumbnail output settings. |
| `watermark` | object | no | see below | Watermark overlay settings. |
| `subdir` | string | no | — | Subfolder under `OUTPUT_DIR`. Slash-separated segments of `[A-Za-z0-9_-]`, max 128 chars. |
| `name` | string | no | source type | File name prefix. `[A-Za-z0-9_-]`, 1–64 chars. |

#### `source.type = "local"` (fswebcam, V4L2 camera)

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `device` | string | — (required) | `/dev/videoN`. Must be mapped into the container and listed in `ALLOWED_DEVICES` when that is set. |
| `resolution` | string | maximal supported | `WxH`, e.g. `1920x1080`. Rejected if the camera does not support it. |
| `skip_frames` | int | `FSWEBCAM_SKIP_FRAMES` (5) | `0`–`100`. Frames discarded so auto-exposure settles. |

Capture uses fswebcam with banner and overlay disabled, at the maximal resolution. The intermediate is lossless PNG (fswebcam's JPEG cannot reach true 100%), so no quality is lost before WebP.

#### `source.type = "rtsp"`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `url` | string | — (required) | `rtsp://host[:port]/path` or `rtsps://…`. **Must not** contain credentials. |
| `username` | string | — | Must be given together with `password`. |
| `password` | string | — | Must be given together with `username`. Redacted from error messages. |

#### `source.type = "gopro"` (Open GoPro HTTP API)

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `host` | string | `10.5.5.9` | GoPro IP. Must be a private or link-local IPv4/IPv6 address. |
| `port` | int | `8080` | `1`–`65535`. |
| `set_photo_mode` | bool | `true` | Switch the camera to the photo preset group before shooting. |
| `delete_after_capture` | bool | `false` | Delete the photo from the camera after it has been downloaded. |

The GoPro must be in photo mode (or `set_photo_mode` must be on), connected to the host or container network, and reachable at `host:port`. Only JPEG photos are accepted. The download is capped at 100 MB.

#### `webp`

| Field | Type | Default (env) | Description |
|-------|------|---------------|-------------|
| `width` | int | `WEBP_WIDTH` (0) | `0`–`16384`. `0` = derive from height or keep original. |
| `height` | int | `WEBP_HEIGHT` (0) | `0`–`16384`. |
| `quality` | int | `WEBP_QUALITY` (80) | `0`–`100`. Ignored when `lossless` is true. |
| `lossless` | bool | `false` | Lossless WebP. |
| `compression_level` | int | `WEBP_COMPRESSION_LEVEL` (4) | `0`–`6`. Higher = smaller and slower. |
| `preset` | string | `WEBP_PRESET` (`default`) | `default`, `picture`, `photo`, `drawing`, `icon`, or `text`. |

If both `width` and `height` are set, the image is fitted inside that box and the aspect ratio is kept. Images are never upscaled.

#### `thumbnail`

| Field | Type | Default (env) | Description |
|-------|------|---------------|-------------|
| `enabled` | bool | `true` | Set `false` to skip the thumbnail. |
| `width` | int | `THUMB_WIDTH` (320) | `0`–`16384`. |
| `height` | int | `THUMB_HEIGHT` (0) | `0`–`16384`. Same fit rules as `webp`. |
| `quality` | int | `THUMB_QUALITY` (70) | `0`–`100`. |

The thumbnail is produced from the main WebP's dimensions and saved as `<base>.thumb.webp`.

#### `watermark`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `true` | Set `false` for an unmarked image. |
| `timezone` | string | `TIMEZONE` (`Europe/Budapest`) | IANA zone name for the timestamp, e.g. `UTC`, `America/New_York`. |

The watermark is drawn in the bottom-right corner:
- the Leafr logo (`LOGO_FILE`, a PNG; its aspect ratio is read from the file),
- a timestamp in the form `2026-10-17 14:20 CEST`, set in Manrope (`FONT_FILE`),
- on a discreet dark box (black, 35 % opacity) behind the text and logo.

Text size scales with the output width.

### Response

```json
{
  "path": "/data/images/cam1/garden_20261017T121500Z_ab12cd34.webp",
  "width": 1280,
  "height": 720,
  "thumbnail_path": "/data/images/cam1/garden_20261017T121500Z_ab12cd34.thumb.webp",
  "thumbnail_width": 320,
  "thumbnail_height": 180,
  "source": "local",
  "captured_at": "2026-10-17T14:15:00+02:00",
  "captured_resolution": "1920x1080"
}
```

- `path` / `thumbnail_path` are absolute paths **inside the container**. With the compose file, `/data/images` is `./data/images` on the host.
- `thumbnail_*` are omitted when the thumbnail is disabled.
- `captured_at` is in the watermark timezone (RFC 3339).
- `captured_resolution` is set for `local` and `gopro` sources.
- File names are `<name or source>_<UTC timestamp>_<random 8 chars>.webp`.
- The original capture and every intermediate file are deleted on success and on failure.

### Errors

Every error uses the same body:

```json
{ "error": { "code": "rtsp_unauthorized", "message": "RTSP server rejected the credentials" } }
```

| HTTP | Code | Meaning |
|------|------|---------|
| 400 | `invalid_request` | Malformed JSON or a field failed validation. |
| 403 | `device_not_allowed` | Device is not in `ALLOWED_DEVICES`. |
| 403 | `device_permission_denied` | Container user cannot open the camera (check `group_add`), or the device is mapped read-only (use `:rwm`, not `:r`). |
| 404 | `device_not_found` | Device node does not exist in the container. |
| 409 | `device_busy` | Camera is in use by another process. |
| 409 | `source_busy` | Another capture from the same source is running. |
| 422 | `image_too_large` | Captured frame exceeds `MAX_PIXELS`. |
| 429 | `too_many_requests` | `MAX_CONCURRENT` captures already running. Retry after `Retry-After` seconds. |
| 500 | `internal_error`, `output_unavailable`, `tmp_unavailable`, `tool_missing`, `convert_failed` | Server-side failure. See the logs. |
| 502 | `device_no_formats` | Device reports no frame sizes (not a capture device). |
| 502 | `camera_error` | fswebcam failed. |
| 502 | `rtsp_unauthorized`, `rtsp_stream_not_found`, `rtsp_unreachable`, `rtsp_error` | RTSP capture failed. |
| 502 | `gopro_unreachable`, `gopro_error`, `gopro_unsupported_media`, `gopro_download_failed` | GoPro API or download failed. |
| 502 | `invalid_capture` | Captured data is not a readable image. |
| 504 | `capture_timeout`, `convert_timeout` | Took longer than `CAPTURE_TIMEOUT_SECONDS`. |

Credentials and secrets are removed from error messages.

## Examples

Replace `localhost:8080` with the host and port of the service. Use `-s | jq` to pretty-print responses if `jq` is installed.

```bash
# Health
curl -s http://localhost:8080/healthz

# Local camera, all defaults (maximal resolution, thumbnail + watermark on)
curl -s -X POST http://localhost:8080/capture \
  -H 'Content-Type: application/json' \
  -d '{"source":{"type":"local","device":"/dev/video0"}}'

# Local camera, explicit resolution, WebP 1280 px wide, named and in a subfolder
curl -s -X POST http://localhost:8080/capture \
  -H 'Content-Type: application/json' \
  -d '{
        "source": {"type":"local","device":"/dev/video0","resolution":"1920x1080"},
        "webp": {"width":1280,"quality":85,"preset":"photo"},
        "thumbnail": {"width":320,"quality":70},
        "subdir":"cam1","name":"garden"
      }'

# RTSP with username and password (credentials are not in the URL)
curl -s -X POST http://localhost:8080/capture \
  -H 'Content-Type: application/json' \
  -d '{
        "source": {"type":"rtsp","url":"rtsp://192.168.1.50:554/stream1",
                   "username":"admin","password":"s3cret"},
        "subdir":"driveway","name":"rtsp"
      }'

# GoPro at the default address (10.5.5.9:8080), deleting the photo from the camera afterwards
curl -s -X POST http://localhost:8080/capture \
  -H 'Content-Type: application/json' \
  -d '{"source":{"type":"gopro","delete_after_capture":true},"name":"gopro"}'

# GoPro at a custom host, lossless WebP, no thumbnail, no watermark, UTC timestamp
curl -s -X POST http://localhost:8080/capture \
  -H 'Content-Type: application/json' \
  -d '{
        "source": {"type":"gopro","host":"172.22.174.51","port":8080},
        "webp": {"lossless":true,"compression_level":6},
        "thumbnail": {"enabled":false},
        "watermark": {"enabled":false}
      }'

# Watermark with a different timezone
curl -s -X POST http://localhost:8080/capture \
  -H 'Content-Type: application/json' \
  -d '{"source":{"type":"local","device":"/dev/video0"},"watermark":{"timezone":"America/New_York"}}'
```


## Environment defaults

`LISTEN_ADDR`, `OUTPUT_DIR`, `ALLOWED_DEVICES`, `MAX_CONCURRENT`, `CAPTURE_TIMEOUT_SECONDS`, `FSWEBCAM_SKIP_FRAMES`, `WEBP_{WIDTH,HEIGHT,QUALITY,COMPRESSION_LEVEL,PRESET}`, `THUMB_{WIDTH,HEIGHT,QUALITY}`.
