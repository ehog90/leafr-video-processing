# leafr-timelapse-generator

Go HTTP/WebSocket service that renders a timelapse video (and optionally an animated thumbnail) from an **image sequence file** written by the Leafr backend. The backend does the data work (which pictures, how long each is shown, where the output goes); this service only runs `ffmpeg`, reports progress live over WebSocket and can cancel a run at any time.

It replaces the in-process ffmpeg step of the backend's timelapse pipeline (see [What this service does and does not do](#what-this-service-does-and-does-not-do)).

- No database, no Redis, no users: **there is no authentication for now.** Do not expose the port outside the trusted network.
- State is in memory only. Finished jobs are kept for `JOB_RETENTION_MINUTES` (default 60) and everything is forgotten on restart; the output files are the durable result.
- **One conversion at a time:** a new request is refused with `409` while one is queued or running.
- The service touches only the media directory (`MEDIA_DIR`, mounted at `/media` exactly as in the backend) and its temp dir.

## Run

```
docker compose up --build        # set MEDIA_UID / MEDIA_GID to the owner of the media dir if not 1000:1000
```

The compose file mounts `/home/regor/leafr/leafr-media/` at `/media` (the same host directory and container path as the backend) and nothing else. Container: non-root user, read-only root filesystem, `/tmp` on tmpfs, no capabilities. The user must be able to write to the media tree (it creates the output directories).

The service listens on `:8090`. The backend (host networking) reaches it at `http://localhost:8090`.

Because the sequence file contains the picture paths **as the backend sees them**, both containers must mount the media directory at the same path (`/media`). Relative paths are also accepted and are resolved against `MEDIA_DIR`.

## Typical flow

```
backend                                   leafr-timelapse-generator
  |== WS /api/ws (one socket, opened at startup, auto-reconnect) ==|
  |<-- {"type":"snapshot","jobs":[...],"active":null}   (on every connect)
  | write /media/.../seq.imageseq                    |
  |-- POST /api/timelapses ------------------------->|  202 {id, status:"queued"}
  |                                                  |  (409 conversion_in_progress while one is active)
  |<-- {"type":"queued","job":{...}}
  |<-- {"type":"started","job":{...}}                    -> backend forwards to its clients
  |<-- {"type":"progress","job":{"status":"encoding","progressPercent":42.3,...}}
  |-- DELETE /api/timelapses/<id> (optional) ------->|  kills ffmpeg, removes partial files
  |<-- {"type":"finished","job":{"status":"completed","result":{...}}}   -> backend forwards to its clients
  | create the Timelapse row from `result`           |
```

## Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `LISTEN_ADDR` | `:8090` | HTTP listen address. |
| `MEDIA_DIR` | `/media` | Root directory. Every input and output path must be inside it. Must exist. |
| `TMP_DIR` | OS temp dir | Where the sanitised ffmpeg list is written (deleted after each run). |
| `FFMPEG_BIN` | `ffmpeg` | ffmpeg executable. |
| `ENCODER_THREADS` | number of CPUs | Threads per ffmpeg process. |
| `AV1_PRESET` | `4` | `libsvtav1` preset (lower = slower, smaller). Tuned for a small SBC. |
| `AV1_CRF` | `32` | `libsvtav1` quality. |
| `THUMB_FORMAT` | `webp` | Default thumbnail format when a request does not set one: `webp` or `avif`. |
| `THUMB_WIDTH` | `600` | Default maximum thumbnail width in px. |
| `THUMB_FPS` | `15` | Thumbnail frame rate. |
| `THUMB_MAX_SECONDS` | `3` | Videos longer than this are sped up to this length for the thumbnail. |
| `THUMB_WEBP_QUALITY` | `70` | WebP thumbnail quality (0-100). |
| `THUMB_AVIF_CRF` | `35` | AVIF thumbnail quality. |
| `JOB_RETENTION_MINUTES` | `60` | How long finished jobs stay queryable. |

Codecs without special settings use ffmpeg's defaults (`libx264`, `libx265`).

## API

| Method | Path | Purpose |
|--------|------|---------|
| `POST` | `/api/timelapses` | Start a timelapse. Returns `202` with the job and a `Location` header, or `409 conversion_in_progress` while another one is active. |
| `GET` | `/api/timelapses` | List retained jobs, newest first. Optional `?status=encoding`. |
| `GET` | `/api/timelapses/{id}` | One job. |
| `DELETE` | `/api/timelapses/{id}` | Cancel a queued or running job. |
| `GET` | `/api/ws` | WebSocket with live job updates. Optional `?jobId=<id>`. |
| `GET` | `/healthz` | Liveness (`200 OK`). |

Bodies are JSON; unknown request fields are rejected with `400`. See [Errors](#errors) for the error format and all codes.

### Request: `POST /api/timelapses`

Only what is needed to render is accepted; everything else about the timelapse (cameras, dates, speed-up factor) is already baked into the sequence file by the backend.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `sequenceFile` | string | yes | - | Path of the image sequence file (see [Sequence file](#sequence-file)). |
| `outputFile` | string | yes | - | Video to create. Must end in `.mp4` or `.mkv`. Parent directories are created. |
| `codec` | string | yes | - | `H264` (`libx264`), `HEVC` (`libx265`, tagged `hvc1`) or `AV1` (`libsvtav1`). Case-insensitive. |
| `width`, `height` | int | yes | - | Bounding box in px (16-16384). The video fits inside it keeping the aspect ratio, without padding, so one side can be smaller. Sizes are rounded down to even numbers (required by `yuv420p`). |
| `fps` | int | no | `30` | Output frame rate: `24`, `30` or `60`. |
| `overwrite` | bool | no | `false` | Replace existing output files. Otherwise an existing output is refused with `409 output_exists`. |
| `deleteSequenceFile` | bool | no | `false` | Delete the sequence file after the job **completed** (kept on failure or cancel so you can retry). |
| `thumbnail` | object | no | none | Also create an animated thumbnail of the finished video. |
| `thumbnail.outputFile` | string | yes, if `thumbnail` | - | Must end in `.webp` or `.avif`, matching the format. |
| `thumbnail.format` | string | no | `THUMB_FORMAT` | `webp` or `avif`. |
| `thumbnail.width` | int | no | `THUMB_WIDTH` | Maximum width in px; never upscales. Height follows the aspect ratio. |

```json
{
  "sequenceFile": "/media/timelapses/tmp/seq_1760000000.imageseq",
  "outputFile": "/media/timelapses/PROD_1/CAM_2/2026/10/17/2026-10-17_14-40-00+0200.mp4",
  "codec": "H264",
  "fps": 30,
  "width": 1920,
  "height": 1080,
  "deleteSequenceFile": true,
  "thumbnail": {
    "outputFile": "/media/timelapses/PROD_1/CAM_2/2026/10/17/thumbs/thumb_2026-10-17_14-40-00+0200.webp",
    "format": "webp",
    "width": 600
  }
}
```

Request errors: see [Request errors](#request-errors-http).

### Job object

Returned by every endpoint and pushed over the WebSocket.

```json
{
  "id": "bf960a476ab3678b",
  "status": "encoding",
  "progressPercent": 42.3,
  "progressIndeterminate": false,
  "request": { "...": "the validated request with absolute paths and defaults applied" },
  "result": null,
  "error": null,
  "createdAt": "2026-10-17T12:59:44Z",
  "startedAt": "2026-10-17T12:59:44Z",
  "finishedAt": null
}
```

| `status` | Meaning |
|----------|---------|
| `queued` | Accepted; the worker picks it up immediately (only one conversion runs at a time). |
| `reading` | Reading the sequence file and checking the pictures. Progress is indeterminate. |
| `preparing` | Creating the output folders and writing the list ffmpeg reads. Progress is indeterminate. |
| `encoding` | ffmpeg is rendering the video. |
| `thumbnail` | ffmpeg is rendering the thumbnail (only if requested). |
| `saving` | Moving the finished files to their final names and deleting the sequence file if requested. Progress is indeterminate. |
| `completed` | Done; `result` is set, `progressPercent` is `100`. |
| `failed` | `error` is set. |
| `cancelled` | Cancelled by the client, or the service shut down. |

`progressPercent` is rounded to 0.1, never decreases and stays below 100 until `completed`. With a thumbnail the video encode covers 0-90 % and the thumbnail 90-100 %; without one the encode covers 0-100 %. `progressIndeterminate` is `true` while the percentage is not known yet (`queued`, `reading`, `preparing`, `saving`).

`result` (on `completed`):

| Field | Description |
|-------|-------------|
| `outputFile`, `relativeOutputFile` | Video path, absolute and relative to `MEDIA_DIR` (forward slashes, no leading `/`). |
| `sizeBytes` | Video size. |
| `durationSec` | Video length (sum of the used pictures' display times). |
| `framesUsed`, `missingFrames` | Pictures rendered / skipped because the file was gone. |
| `thumbnailFile`, `relativeThumbnailFile`, `thumbnailSizeBytes`, `thumbnailDurationSec` | Only with a thumbnail. |
| `sequenceFileDeleted` | `true` when `deleteSequenceFile` was set and the deletion worked. |

A job's `error` is set when it `failed`, and also when it was `cancelled` by a service shutdown. The full list of codes is in [Errors](#errors).

### Errors

There are two kinds of errors, and they use the same `{"code","message"}` pair. The `code` is stable and meant for programs; the `message` is for humans and logs and may change.

1. **Request errors**: the HTTP request itself was refused. Nothing was queued.
2. **Job errors**: the conversion was accepted but did not finish. They arrive in the job's `error` field (REST and WebSocket `finished` event); the HTTP request that created the job already returned `202`.

#### Request errors (HTTP)

Every error response, including unknown routes and wrong methods, has the body `{"error":{"code":"...","message":"..."}}` and `Content-Type: application/json`. The WebSocket endpoint uses the same format for errors detected before the upgrade (`not_found` for an unknown `?jobId=`, `shutting_down`); a malformed upgrade request is rejected by the WebSocket library with a plain-text `4xx`.

| Status | Code | Cause | What to do |
|--------|------|-------|------------|
| 400 | `invalid_request` | Malformed JSON, body over 1 MiB, unknown field, data after the JSON body, or an invalid value (codec, fps, size, thumbnail settings, file extension, output equal to input). The message names the field. | Fix the request. Do not retry unchanged. |
| 400 | `invalid_path` | A path is outside `MEDIA_DIR`, resolves outside it through a symlink, or is a directory. | Fix the path. |
| 404 | `sequence_not_found` | `sequenceFile` does not exist or is not a regular file. | Write the file first. |
| 404 | `not_found` | Unknown job id, unknown `?jobId=` on the WebSocket, or unknown endpoint. | |
| 405 | `method_not_allowed` | Known path, wrong method. | |
| 409 | `conversion_in_progress` | Another conversion is queued or running; the message names its id. | Wait for its `finished` event or cancel it, then retry. See [One conversion at a time](#one-conversion-at-a-time). |
| 409 | `output_exists` | An output file exists and `overwrite` is not set. | Use another path or set `overwrite`. |
| 409 | `already_finished` | `DELETE` on a `completed` or `failed` job. | Nothing to cancel. |
| 503 | `shutting_down` | The service is stopping. | Retry after it is back. |
| 500 | `internal_error` | Unexpected server error; the message is always just `internal error` and the details are in the service log. | Retry once; report if it persists. |

The field checks happen first; the existence of the sequence file, symlink escapes and existing outputs are checked right after, still before the `202`.

#### Job errors

`failed` jobs carry one of these codes. The `message` has the details (for `encode_failed` it ends with ffmpeg's last error output).

| Code | Cause | Retry? |
|------|-------|--------|
| `sequence_not_found` | The sequence file was removed after the request was accepted. | Write it again, then resubmit. |
| `sequence_unreadable` | The sequence file cannot be opened or read. | Check permissions, then resubmit. |
| `invalid_sequence` | Unsupported directive, invalid path or duration, or a picture path outside `MEDIA_DIR` (also through symlinks). The message names the line. | No: the backend generated a bad list. |
| `not_enough_frames` | Fewer than two pictures exist (zero or one, or the others are missing). The message gives both counts. | No: nothing to animate. |
| `encode_failed` | ffmpeg exited with an error (corrupt picture, full disk, unsupported option, killed by the OS). | Maybe: read the message. |
| `ffmpeg_unavailable` | The ffmpeg binary could not be started. | No: deployment problem (`FFMPEG_BIN`). |
| `output_unwritable` | Cannot create the output directory, or cannot write or rename the output (permissions, read-only, full disk). | After fixing the cause (check `MEDIA_UID`/`MEDIA_GID`). |
| `output_exists` | An output appeared while the job ran and `overwrite` was not set. | Resubmit with `overwrite` or another path. |
| `invalid_path` | An output directory resolves outside `MEDIA_DIR` through a symlink. | Fix the path. |
| `internal_error` | Anything else, including a recovered panic. | Retry once; check the log. |

A `failed` job leaves no partial output: the `.partial` files and the temp list are removed. Existing files at the output paths are only replaced in the final rename step, after everything succeeded.

`cancelled` jobs normally have **no** `error`: a client cancelled them. If the service was shut down while the job was queued or running, the job is `cancelled` with `error.code` `service_shutdown` (the job is lost after the restart, since state is in memory; the consumer should resubmit it).

### Cancellation

`DELETE /api/timelapses/{id}`:

- `queued`: only exists for a moment before the worker picks the job up; cancelled immediately and never started.
- Running (`reading`, `preparing`, `encoding`, `thumbnail`, `saving`): the ffmpeg process is killed, the `.partial` files and the temp list are removed, and the job ends as `cancelled`. The response waits up to 5 s for that and returns `200` with the final job (`202` if it is still winding down; the WebSocket reports the end state).
- Already `cancelled`: `200` (idempotent). `completed` or `failed`: `409 already_finished`.
- Unknown id: `404`.

Cancelling never touches existing output files: the new video is written to `<outputFile>.partial` and only renamed once the whole job succeeded, so an overwrite is atomic and a cancelled run cannot leave a truncated video. On `SIGTERM`/`SIGINT` the service cancels all jobs the same way; they end as `cancelled` with error code `service_shutdown`.

### WebSocket: `GET /api/ws`

The single channel for every state change. The consumer opens **one** WebSocket for the whole lifetime of the consumer service (not one per job) and keeps it open; see [Consumer rules](#consumer-rules). Any origin is accepted (the service has no authentication; keep it on the trusted network).

Server-to-client only: a client that sends a data message is disconnected (close code `1008`). The optional `?jobId=<id>` filter limits events to one job (`404` before the upgrade if unknown); it is meant for debugging, not for normal use. Messages are JSON text frames, always one of these types:

| `type` | When | Content |
|--------|------|---------|
| `snapshot` | **Immediately after every connect, including every reconnect**, before any other message | `jobs`: all retained jobs, newest first. `active`: the queued or running conversion, absent when idle. |
| `queued` | A conversion was accepted | `job` |
| `started` | The conversion began (`status` `reading`) | `job` |
| `progress` | The stage (`encoding`, `thumbnail`) or the rounded percentage changed | `job` |
| `finished` | The conversion ended: `completed`, `failed` or `cancelled` | `job` (with `result` or `error`) |

```json
{"type":"snapshot","jobs":[{ "...": "job" }],"active":{ "...": "job" }}
{"type":"started","job":{ "...": "job" }}
{"type":"progress","job":{"id":"bf960a476ab3678b","status":"encoding","progressPercent":42.3,"progressIndeterminate":false}}
{"type":"finished","job":{ "...": "job" }}
```

(`job` is always the full job object; the `progress` example is abbreviated.) The `snapshot` gives the current status on every (re)connect, so a consumer that was disconnected never misses a start, a progress step or the outcome: it replaces whatever state it had. ffmpeg reports about twice a second; a `progress` event is sent only when the rounded percentage or the status changes. The server pings every 25 s. A client that cannot keep up (256 queued events) is disconnected with close code `1013`, as are all clients on shutdown; reconnect and use the new snapshot.

```
websocat "ws://localhost:8090/api/ws"
```

### Consumer rules

The service that consumes this API (the backend) must:

1. **Use a single WebSocket for all state changes** of all conversions, opened once at startup, not per request or per job. Do not poll `GET /api/timelapses/{id}` for progress.
2. **Reconnect automatically** whenever the socket closes or errors (service restart, network problem, `1013`), with a short backoff (for example 1 s doubling to 30 s, reset after a successful connect). While disconnected, state is unknown.
3. **Treat every `snapshot` as the source of truth** and replace its stored state with it. This is how a `finished` event missed during an outage is recovered: the job is `completed`, `failed` or `cancelled` in the snapshot. Process the `result` of a completed job exactly once (match on the job `id`), because the same job appears in every snapshot for `JOB_RETENTION_MINUTES`.
4. **Distribute `started` and `finished` (and, if wanted, `queued` and `progress`) to its own clients**, and send the current state (from the latest snapshot or event) to clients that connect later, so every client sees whether a conversion is running and how far it is.
5. **Never start a new conversion while one is in progress** (see below).
6. **Handle every outcome of `finished`**: `completed` (use `result`), `failed` (use `error.code`, see [Job errors](#job-errors); retry only where the table says so), and `cancelled` (no `error`: a client cancelled it; `error.code` `service_shutdown`: the service stopped, resubmit after it is back). For HTTP errors, switch on `error.code`, not on the message or only the status.

### One conversion at a time

Only one conversion may be active (`queued`, `reading`, `preparing`, `encoding`, `thumbnail` or `saving`) at any moment. While one is active, `POST /api/timelapses` is refused with `409 conversion_in_progress` and the message names the blocking job; nothing is queued behind it. A new one is accepted as soon as the active job reached `completed`, `failed` or `cancelled` (the `finished` event).

The consumer should enforce the same rule on its side: use the `active` field of the latest snapshot (or the `queued`/`started`/`finished` events) to know whether a conversion is running, do not offer or accept a new request in the meantime, and offer cancel instead (`DELETE /api/timelapses/{id}`). A `409 conversion_in_progress` response means its view was stale; it should wait for the next event.

## Sequence file

An [ffmpeg concat list](https://ffmpeg.org/ffmpeg-formats.html#concat-1) written by the backend: one `file` line per picture, each followed by its display time.

```
ffconcat version 1.0
file '/media/cameras/PROD_1/CAM_2/2026/10/16/a.webp'
duration 0.033333
file '/media/cameras/PROD_1/CAM_2/2026/10/16/b.webp'
duration 0.1
```

The service does not run this file through ffmpeg directly. It parses it, validates it and writes its own list:

- Accepted lines: `ffconcat version ...`, `file <path>`, `duration <seconds>`, blank lines and `#` comments. Anything else (`option`, `inpoint`, `ffconcat` includes, protocol tricks) is rejected with `invalid_sequence`.
- Every picture path (quoted with `'...'` as ffmpeg expects, or plain) must be inside `MEDIA_DIR`, also after resolving symlinks.
- Pictures that no longer exist are skipped and their duration is added to the next existing picture (to the last one if the missing pictures are at the end), so the video length is preserved. `missingFrames` reports the count.
- An entry without a `duration` (for example a repeated last file) carries no time and is dropped. The service itself repeats the last picture when it writes its list, because the concat demuxer ignores the duration of the last entry otherwise, and limits the output to the exact total length with `-t`.
- The total of the durations is the video length and the denominator of the progress.

Producing the right durations (acceleration, the cap on long gaps, merging pictures shorter than one output frame) is the backend's job.

## Pipeline

1. **reading** (status set by the manager before the pipeline runs): parse the sequence file and drop missing pictures.
2. **preparing**: create the output directories (and check them for symlink escapes), write the sanitised list to `TMP_DIR`.
3. **encoding**: `ffmpeg -f concat -safe 0 -i <list> -an -vf scale=w=W:h=H:force_original_aspect_ratio=decrease:force_divisible_by=2 -pix_fmt yuv420p -r <fps> -t <total> -c:v <encoder> ... <outputFile>.partial`. Progress comes from ffmpeg's `-progress` stream (`out_time_us` / total).
   - `H264`: `libx264`, default settings. `HEVC`: `libx265 -tag:v hvc1`. `AV1`: `libsvtav1 -preset $AV1_PRESET -crf $AV1_CRF -svtav1-params lp=$ENCODER_THREADS`.
   - Container is chosen from the extension: `.mp4` (with `+faststart`) or `.mkv`.
4. **thumbnail** (optional): `ffmpeg -i <video partial> -vf [setpts,]fps=15,scale='min(W,iw)':-2` into an endlessly looping animated WebP (`libwebp`) or AVIF (`libsvtav1`). A video longer than `THUMB_MAX_SECONDS` is sped up to that length.
5. **saving**: rename the partial files to their final names (video first), read the sizes, optionally delete the sequence file. Any failure removes the partial files and the temp list.

## What this service does and does not do

Does: the work of steps 7-9 of the backend's timelapse process (ffmpeg concat encode, animated thumbnail, file output), plus the one-conversion-at-a-time guard, cancellation and progress.

Does not (stays in the backend): choosing pictures and the date range, computing the sequence (sorting, acceleration, frame merging), the `Timelapse` row, the daily-thumbnail handling, the schedule, and serving or downloading videos.

Differences from the backend's current ffmpeg step:

- `VP8` / `VP9` are not accepted. The old code silently encoded them as H.264 in an MKV container; ask for `H264` or `AV1` explicitly instead.
- The backend's sequence file belongs to the backend: it is only deleted on success and only with `deleteSequenceFile`. The service's own sanitised copy is always removed.
- Even output dimensions are enforced (`force_divisible_by=2`).

## Logging

Everything is logged to stdout (`docker logs`), one line per event, with an emoji to scan quickly. Every line about a conversion carries the job id.

| Emoji | Event |
|-------|-------|
| 📥 | A conversion was accepted (paths, codec, fps, size, thumbnail). |
| 🚀 | It started. |
| 📖 📋 🎬 🖼️ 💾 | Stage changes: `reading`, `preparing`, `encoding`, `thumbnail`, `saving`. |
| ⏳ | Progress, every 10 %. |
| 📄 ⚠️ | The sequence was read (pictures, video length); warning when pictures were missing. |
| 🎞️ | The exact ffmpeg arguments (useful when reproducing a failure by hand). |
| 📦 🗑️ 🧹 | Outputs written; sequence file deleted; partial files removed after a failure or cancel. |
| ✅ | Completed (duration, pictures, sizes). |
| ❌ | Failed, with the error code and message. |
| 🛑 | Cancelled: a cancel request, and the cancelled job (with `service_shutdown` when the service stopped). |
| ⏸️ 🚫 ↩️ | A request refused because a conversion is active, an invalid request, a refused cancel. |
| 🔌 🐌 | A WebSocket client connected or disconnected; a client that could not keep up was dropped. |

Example of one run:

```
📥 timelapse bf960a476ab3678b queued: /media/tmp/seq.imageseq -> /media/out/a.mp4 (H264, 30 fps, fit 1920x1080, thumbnail webp max 600px)
🚀 timelapse bf960a476ab3678b started
📄 sequence read: 59 pictures, 6.0s of video (timelapse bf960a476ab3678b)
⚠️ 1 pictures in the sequence no longer exist; their time was added to the neighbouring pictures (timelapse bf960a476ab3678b)
🎬 timelapse bf960a476ab3678b encoding
⏳ timelapse bf960a476ab3678b encoding 30%
🖼️ timelapse bf960a476ab3678b thumbnail
📦 output written: /media/out/a.mp4 (591.7 KiB) (timelapse bf960a476ab3678b)
✅ timelapse bf960a476ab3678b completed in 1.7s: 6.0s video, 59 pictures (1 missing), 591.7 KiB, thumbnail 111.2 KiB
```

## Development

```
go vet ./...
go test ./...          # add -race with CGO_ENABLED=1
go build -o leafr-timelapse-generator .
```

ffmpeg is needed only to actually render; the unit tests use a fake runner. Source layout:

| File | Content |
|------|---------|
| `main.go`, `config.go` | Wiring, graceful shutdown, `-healthcheck`, environment. |
| `server.go` | HTTP routes and the WebSocket endpoint. |
| `manager.go` | Job store, worker, one-conversion-at-a-time rule, cancellation, event fan-out, retention. |
| `pipeline.go` | The render steps and the partial-file handling. |
| `ffmpeg.go` | ffmpeg argument building and the process runner with progress parsing. |
| `sequence.go` | Sequence file parser/validator and the sanitised list writer. |
| `paths.go` | Confinement of every path to `MEDIA_DIR`. |
| `types.go` | Request/response/job types, request validation. |
