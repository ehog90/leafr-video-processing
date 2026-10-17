# leafr-image-capture-convert

Go HTTP service: captures one frame from a local V4L2 camera (`fswebcam`), an RTSP stream (`ffmpeg`, user/password) or a GoPro (Open GoPro HTTP API), converts it to the configured output format (`webp` is the only implemented format; see [`image`](#image)), adds a Leafr watermark to the main image and returns the image as base64. Nothing is stored on disk: all work happens in a temporary directory that is removed after each request.

## Run

```
docker compose up --build        # set VIDEO_GID to the host `video` group id if not 44
```

Every host V4L2 video device (`/dev/videoN`, char major 81) is visible to the container: `/dev` is bind-mounted and a `device_cgroup_rules` entry grants `rwm` access to major 81 only, so cameras plugged in later are picked up without editing the compose file. Other host device nodes are visible but not accessible. The app runs as a non-root user, read-only rootfs, no capabilities. `ALLOWED_DEVICES` (comma list) restricts which `/dev/videoN` can be requested; the compose file sets it to `/dev/video0`, so other cameras are exposed but refused until they are added to it.

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
| `image` | object | no | see below | Main image output settings (`format` and `webp`). |
| `thumbnail` | object | no | see below | Thumbnail output settings (`enabled`, `format` and `webp`). |
| `watermark` | object | no | see below | Watermark overlay settings. |

#### `source.type = "local"` (fswebcam, V4L2 camera)

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `device` | string | — (required) | `/dev/videoN`. Must exist in the container (every host video device does) and be listed in `ALLOWED_DEVICES` when that is set. |
| `resolution` | string | maximal supported | `WxH`, e.g. `1920x1080`. Rejected if the camera does not support it. |
| `skip_frames` | int | `FSWEBCAM_SKIP_FRAMES` (5) | `0`–`100`. Frames discarded so auto-exposure settles. |

Capture uses fswebcam with banner and overlay disabled, at the maximal resolution. The intermediate is lossless PNG (fswebcam's JPEG cannot reach true 100%), so no quality is lost before WebP.

**Mains flicker (banding):** before each capture the service sets the camera's V4L2 `power_line_frequency` control to the local mains frequency, taken from the `POWER_LINE_FREQUENCY` environment variable (`50` for Europe, the default, or `60` for the US and most of the Americas). This suppresses the horizontal banding caused by lighting flicker. The setting is process-wide and is not part of the request. If the camera does not support the control, a warning is logged and the capture continues without it.

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

#### `image`

Main image output. The image and the thumbnail are configured separately; each has its own `format` and its own format-specific options.

| Field | Type | Default (env) | Description |
|-------|------|---------------|-------------|
| `format` | string | `IMAGE_FORMAT` (`webp`) | Output format. Currently only `webp` is implemented; any other value is rejected with `400 invalid_request`. |
| `webp` | object | — | WebP options, used when `format` is `webp`. Omitted fields use the defaults below. |

WebP options (`image.webp` and `thumbnail.webp` take the same fields):

| Field | Type | Default (env) | Description |
|-------|------|---------------|-------------|
| `quality` | int | `WEBP_QUALITY` (80) for `image`, `THUMB_QUALITY` (70) for `thumbnail` | `0`–`100`. Ignored when `lossless` is true. |
| `lossless` | bool | `WEBP_LOSSLESS` / `THUMB_LOSSLESS` (`false`) | Lossless WebP. |
| `compression_level` | int | `WEBP_COMPRESSION_LEVEL` / `THUMB_COMPRESSION_LEVEL` (4) | `0`–`6`. Higher = smaller and slower. |
| `preset` | string | `WEBP_PRESET` / `THUMB_PRESET` (`default`) | `default`, `picture`, `photo`, `drawing`, `icon`, or `text`. |

The output is always exactly 1280x720 (16:9). The capture keeps its aspect ratio and is scaled to fit inside that frame. Black bars fill the rest (letterboxing, so 4:3 sources get bars on the left and right). Smaller sources are scaled up to fill the frame.

#### `thumbnail`

| Field | Type | Default (env) | Description |
|-------|------|---------------|-------------|
| `enabled` | bool | `true` | Set `false` to skip the thumbnail. |
| `format` | string | `THUMB_FORMAT` (`webp`) | Output format. Same rules as `image.format`. |
| `webp` | object | — | WebP options for the thumbnail (see the table above). |

The thumbnail is cut from the same 1280x720 letterboxed frame, so it is also exactly 1280x720 and has the same black bars. It never has the watermark. It is returned as base64 in `thumbnail`.

#### `watermark`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `true` | Set `false` for an unmarked main image. Thumbnails never have the watermark. |
| `timezone` | string | `TIMEZONE` (`Europe/Budapest`) | IANA zone name for the timestamp, e.g. `UTC`, `America/New_York`. |

The watermark is drawn in the bottom-left corner, with a margin from the edges:
- the Leafr logo (`LOGO_FILE`, a PNG; its aspect ratio is read from the file),
- a timestamp in the form `2026-10-17 14:20 CEST`, set in Manrope (`FONT_FILE`),
- on a discreet dark panel (black, 35 % opacity) with rounded corners, behind the text and logo.

The watermark has a fixed size of 16 px text (logo 27 px high) regardless of the image size, so it stays the same size on every capture. The logo is resampled with a Catmull-Rom filter before compositing, and the panel edges are antialiased, so the watermark stays smooth at any output size. If the panel does not fit in the image, the image is returned without a watermark.

Each successful capture writes one log line, for example `captured source=local 1920x1080 bytes=148392 took=2.4s`. This line does not include credentials or the camera or GoPro address.

### Response

```json
{
  "image": "<base64-encoded WebP>",
  "format": "webp",
  "thumbnail": "<base64-encoded WebP>",
  "thumbnail_format": "webp",
  "width": 1280,
  "height": 720,
  "size": 148392,
  "thumbnail_width": 1280,
  "thumbnail_height": 720,
  "thumbnail_size": 12480,
  "source": "local",
  "captured_at": "2026-10-17T14:15:00+02:00",
  "captured_resolution": "1920x1080"
}
```

- `image` is the converted main image and is always returned. `thumbnail` is the thumbnail. Both are standard base64 (RFC 4648) of the files in the format named by `format` / `thumbnail_format` (currently always `webp`); decode them to get the image bytes.
- `format` / `thumbnail_format` name the output format of `image` / `thumbnail` (`thumbnail_format` is omitted when the thumbnail is disabled).
- `size` / `thumbnail_size` are the WebP byte sizes of the decoded images, not the length of the base64 strings.
- `thumbnail_*` fields are omitted when the thumbnail is disabled.
- Base64 makes the JSON about 33 % larger than the image. Large outputs, such as lossless images at high resolution, produce large responses.
- `captured_at` is in the watermark timezone (RFC 3339).
- `captured_resolution` is set for `local` and `gopro` sources.
- The raw capture and temporary outputs are deleted on success and on failure. Nothing is kept on disk.

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
  "image": { "format": "webp", "webp": { "quality": 85 } },
  "thumbnail": { "format": "webp", "webp": { "quality": 70 } },
  "watermark": { "enabled": false }
}
```

Only `source` is required. It must contain the source-specific information
described above: a local device path, an RTSP URL and optional credentials, or
GoPro connection options. Do not put RTSP credentials in the URL. `image`,
`thumbnail`, and `watermark` are optional.

On success, the response includes the image as base64 WebP (`image`, plus `thumbnail` when enabled), dimensions, byte sizes, source type, capture time, and (where available) source resolution. Decode the base64 fields to get the image bytes. Nothing is written to disk, so the image bytes in the response are the only copy: the caller is responsible for storing them.

When adapting the result to a record such as `CameraPicture`, the caller can map `captured_at` to its capture timestamp, `width`/`height` to the image dimensions, `size`/`thumbnail_size` to byte sizes, and the decoded `image`/`thumbnail` bytes to the stored or forwarded image. The caller supplies its own camera and production IDs; those IDs are not sent to this service.

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
| 500 | `internal_error`, `tmp_unavailable`, `tmp_permission_denied`, `tool_missing`, `convert_failed` | Server-side failure. `tmp_unavailable`/`tmp_permission_denied`: the temporary directory (`TMP_DIR`) is missing or not writable. See the logs. |
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

# Local camera, all defaults (1280x720 output, thumbnail + watermark on); returns base64 only
curl -s -X POST http://localhost:8080/capture \
  -H 'Content-Type: application/json' \
  -d '{"source":{"type":"local","device":"/dev/video0"}}'

# Local camera with an explicit source resolution, WebP quality 85 for the image
curl -s -X POST http://localhost:8080/capture \
  -H 'Content-Type: application/json' \
  -d '{
        "source": {"type":"local","device":"/dev/video0","resolution":"1920x1080"},
        "image": {"webp": {"quality":85,"preset":"photo"}},
        "thumbnail": {"webp": {"quality":70}},
      }'

# RTSP with username and password (credentials are not in the URL)
curl -s -X POST http://localhost:8080/capture \
  -H 'Content-Type: application/json' \
  -d '{
        "source": {"type":"rtsp","url":"rtsp://192.168.1.50:554/stream1",
                   "username":"admin","password":"<password>"}
      }'

# GoPro at the default address (10.5.5.9:8080), deleting the photo from the camera afterwards
curl -s -X POST http://localhost:8080/capture \
  -H 'Content-Type: application/json' \
  -d '{"source":{"type":"gopro","delete_after_capture":true}}'

# GoPro at a custom host, lossless WebP, no thumbnail, no watermark, UTC timestamp
curl -s -X POST http://localhost:8080/capture \
  -H 'Content-Type: application/json' \
  -d '{
        "source": {"type":"gopro","host":"172.22.174.51","port":8080},
        "image": {"format":"webp","webp": {"lossless":true,"compression_level":6}},
        "thumbnail": {"enabled":false},
        "watermark": {"enabled":false}
      }'

# Watermark with a different timezone
curl -s -X POST http://localhost:8080/capture \
  -H 'Content-Type: application/json' \
  -d '{"source":{"type":"local","device":"/dev/video0"},"watermark":{"timezone":"America/New_York"}}'
```


## Environment defaults

`LISTEN_ADDR`, `ALLOWED_DEVICES`, `MAX_CONCURRENT`, `CAPTURE_TIMEOUT_SECONDS`, `FSWEBCAM_SKIP_FRAMES`, `POWER_LINE_FREQUENCY` (`50` or `60`), `TIMEZONE`, `MAX_PIXELS`.

Output defaults, set separately for the main image and the thumbnail:

| Output | Format | Quality | Lossless | Compression level | Preset |
|--------|--------|---------|----------|-------------------|--------|
| Image | `IMAGE_FORMAT` (`webp`) | `WEBP_QUALITY` (80) | `WEBP_LOSSLESS` (`false`) | `WEBP_COMPRESSION_LEVEL` (4) | `WEBP_PRESET` (`default`) |
| Thumbnail | `THUMB_FORMAT` (`webp`) | `THUMB_QUALITY` (70) | `THUMB_LOSSLESS` (`false`) | `THUMB_COMPRESSION_LEVEL` (4) | `THUMB_PRESET` (`default`) |
