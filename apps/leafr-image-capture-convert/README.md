# leafr-image-capture-convert

Go HTTP service: captures one frame from a local V4L2 camera (`fswebcam`), an RTSP stream (`ffmpeg`, user/password) or a GoPro (Open GoPro HTTP API), converts it to WebP (+ thumbnail), adds a Leafr watermark to the main image and returns the image as base64. Files are written to `OUTPUT_DIR` only when `save` is true.

## Run

```
docker compose up --build        # set VIDEO_GID to the host `video` group id if not 44
```

Devices are mapped one by one from the host in `docker-compose.yml` (`rwm`, V4L2 needs read-write); the app runs as a non-root user, read-only rootfs, no capabilities. `ALLOWED_DEVICES` (comma list) restricts which `/dev/videoN` can be requested.

The output directory (`./data/images` on the host) must be writable by uid `10001`, the container user. On Linux, run once before starting:

```
mkdir -p data/images && sudo chown -R 10001:10001 data/images
```

If it is not writable, the service exits at startup with a message naming the directory.

## API

| Method | Path       | Purpose                                  |
|--------|------------|------------------------------------------|
| POST   | `/capture` | Capture one image, convert, return base64 WebP |
| GET    | `/healthz` | Liveness (`200 OK`)                      |

All bodies are JSON. Unknown fields are rejected with `400 invalid_request`.

### Request: `POST /capture`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `source` | object | yes | — | Where the frame comes from. See per-source fields below. |
| `webp` | object | no | see below | WebP output settings. |
| `thumbnail` | object | no | see below | Thumbnail output settings. |
| `watermark` | object | no | see below | Watermark overlay settings. |
| `save` | bool | no | `false` | Also write the WebP file(s) to `OUTPUT_DIR` (under `subdir`). The base64 image is always returned. |
| `subdir` | string | no | — | Subfolder under `OUTPUT_DIR`, used only when `save` is true. Slash-separated segments of `[A-Za-z0-9_-]`, max 128 chars. |
| `name` | string | no | source type | File name prefix, used only when `save` is true. `[A-Za-z0-9_-]`, 1–64 chars. |

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

The thumbnail is scaled from the converted image and never has the watermark. It is returned as base64 in `thumbnail` and, when `save` is true, saved as `<base>.thumb.webp`.

#### `watermark`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `true` | Set `false` for an unmarked main image. Thumbnails never have the watermark. |
| `timezone` | string | `TIMEZONE` (`Europe/Budapest`) | IANA zone name for the timestamp, e.g. `UTC`, `America/New_York`. |

The watermark is drawn in the bottom-left corner, with a margin from the edges:
- the Leafr logo (`LOGO_FILE`, a PNG; its aspect ratio is read from the file),
- a timestamp in the form `2026-10-17 14:20 CEST`, set in Manrope (`FONT_FILE`),
- on a discreet dark panel (black, 35 % opacity) with rounded corners, behind the text and logo.

The watermark has a fixed size of 22 px text (logo 37 px high) regardless of the image size, so it stays the same size on every capture. The logo is resampled with a Catmull-Rom filter before compositing, and the panel edges are antialiased, so the watermark stays smooth at any output size. If the panel does not fit in the image, the image is returned without a watermark.

Each successful capture writes one log line, for example `captured source=local 1920x1080 bytes=148392 saved=false took=2.4s`. This line does not include credentials or the camera or GoPro address.

### Response

```json
{
  "image": "<base64-encoded WebP>",
  "thumbnail": "<base64-encoded WebP>",
  "width": 1280,
  "height": 720,
  "size": 148392,
  "thumbnail_width": 320,
  "thumbnail_height": 180,
  "thumbnail_size": 12480,
  "source": "local",
  "captured_at": "2026-10-17T14:15:00+02:00",
  "captured_resolution": "1920x1080"
}
```

With `"save": true`, the response also contains `path` and, when the thumbnail is enabled, `thumbnail_path`.

- `image` is the converted main image and is always returned. `thumbnail` is the thumbnail. Both are standard base64 (RFC 4648) of WebP files; decode them to get the image bytes.
- `path` / `thumbnail_path` are only present when `save` is `true`. They are absolute paths inside the container; with the compose file, `/data/images` is `./data/images` on the host.
- `size` / `thumbnail_size` are the WebP byte sizes of the decoded images, not the length of the base64 strings.
- `thumbnail_*` fields are omitted when the thumbnail is disabled.
- Base64 makes the JSON about 33 % larger than the image. Large outputs, such as lossless images at high resolution, produce large responses.
- `captured_at` is in the watermark timezone (RFC 3339).
- `captured_resolution` is set for `local` and `gopro` sources.
- When `save` is `true`, file names are `<name or source>_<UTC timestamp>_<random 8 chars>.webp` and `<same base>.thumb.webp`.
- The raw capture and temporary outputs are deleted on success and on failure. Saved files (when `save` is `true`) are kept. With `save` false, nothing is kept on disk.

## Calling from another application

This service only captures and processes images. It does not depend on the calling
application's Prisma models and does not accept or need a `Production`,
`productionId`, or `cameraId`. The caller sends the camera's capture connection
details (`source`) and any desired output options; it remains responsible for
associating the result with its own camera or production records.

For example, an application with an RTSP camera can send:

```json
{
  "source": {
    "type": "rtsp",
    "url": "rtsp://192.168.1.50:554/stream1",
    "username": "camera-user",
    "password": "camera-password"
  },
  "name": "camera-123",
  "webp": { "width": 1920, "quality": 85 },
  "thumbnail": { "width": 320, "quality": 70 },
  "watermark": { "enabled": false }
}
```

Only `source` is required. It must contain the source-specific information
described above: a local device path, an RTSP URL and optional credentials, or
GoPro connection options. Do not put RTSP credentials in the URL. `name`,
`webp`, `thumbnail`, and `watermark` are optional; omit `name` to use the source
type as the filename prefix, which is only used when `save` is `true`.

On success, the response includes the image as base64 WebP (`image`, plus `thumbnail` when enabled), dimensions, byte sizes, source type, capture time, and (where available) source resolution. Decode the base64 fields to get the image bytes. The example above returns the image without writing it to disk. Add `"save": true` to also write the WebP files to `OUTPUT_DIR`; the response then includes `path` and `thumbnail_path`. Those paths are inside the capture service's container, so the caller can only read them if it shares the `OUTPUT_DIR` volume.

When adapting the result to a record such as `CameraPicture`, the caller can map `captured_at` to its capture timestamp, `width`/`height` to the image dimensions, `size`/`thumbnail_size` to byte sizes, and the decoded `image`/`thumbnail` bytes to the stored or forwarded image. Use `path`/`thumbnail_path` only when `save` is `true` and the caller can read the same `OUTPUT_DIR`. The caller supplies its own camera and production IDs; those IDs are not sent to this service.

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
| 500 | `internal_error`, `output_unavailable`, `output_permission_denied`, `tmp_unavailable`, `tool_missing`, `convert_failed` | Server-side failure. `output_permission_denied`: with `save` true, the service user cannot write `OUTPUT_DIR`; fix host ownership (see below). See the logs. |
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

# Local camera, all defaults (maximal resolution, thumbnail + watermark on); returns base64 only, nothing is saved
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
        "save":true,"subdir":"cam1","name":"garden"
      }'

# RTSP with username and password (credentials are not in the URL)
curl -s -X POST http://localhost:8080/capture \
  -H 'Content-Type: application/json' \
  -d '{
        "source": {"type":"rtsp","url":"rtsp://192.168.1.50:554/stream1",
                   "username":"admin","password":"s3cret"},
        "save":true,"subdir":"driveway","name":"rtsp"
      }'

# GoPro at the default address (10.5.5.9:8080), deleting the photo from the camera afterwards
curl -s -X POST http://localhost:8080/capture \
  -H 'Content-Type: application/json' \
  -d '{"source":{"type":"gopro","delete_after_capture":true},"save":true,"name":"gopro"}'

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
