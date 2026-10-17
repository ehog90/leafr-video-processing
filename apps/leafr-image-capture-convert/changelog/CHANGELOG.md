# Changelog

All notable changes to `leafr-image-capture-convert` are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [1.1.0]

### Changed (breaking)

- `POST /capture` no longer returns the image and thumbnail as base64. The converted files are written into the shared media directory and the response returns their paths relative to it. See [README.md](../README.md#response).
- README: the intro, Run, API table, thumbnail, response, calling and example sections now describe writing to `MEDIA_DIR` instead of returning base64.

### Added

- `image_path` and `thumbnail_path` response fields. Both are relative to `MEDIA_DIR`, with forward slashes.
- `MEDIA_DIR` environment variable (required). The service refuses to start if it is not an existing directory. The compose file mounts the shared leafr media directory there.
- `media_unavailable` error (500) for when `MEDIA_DIR` cannot be written.
- Output files `converts/<id>_.<ext>` and `converts/<id>_thumbnail.<ext>`, where `<id>` is `YYYYMMDD-HHMMSS-<8 hex>`. The consumer deletes both files after it has stored them.
- `.dockerignore` rules that keep tests, documentation, secrets, and local binaries out of the build context.

### Removed

- The `image` and `thumbnail` base64 response fields.

### Migration

For consumers of the `POST /capture` response:

| Before | After |
|--------|-------|
| `image` (base64) | `image_path`. Read `MEDIA_DIR/<image_path>`. |
| `thumbnail` (base64) | `thumbnail_path`. Read `MEDIA_DIR/<thumbnail_path>`. |
| Decode base64 to bytes | Read the file. No decoding is needed. |
| `size`, `thumbnail_size` | Same values as before. |
| `width`, `height`, `captured_at`, `source`, `captured_resolution` | Unchanged. |

Before upgrading:

1. **Shared volume.** The consumer must mount the same host directory as `MEDIA_DIR` and read the same relative paths. Both services must run on the same host.
2. **Cleanup.** The service never deletes successful outputs. After it has stored the files, the consumer must delete `image_path` and `thumbnail_path`. A failed conversion removes its own files.
3. **Optional thumbnail.** `thumbnail_path` and its related fields are omitted when `thumbnail.enabled` is `false`.
