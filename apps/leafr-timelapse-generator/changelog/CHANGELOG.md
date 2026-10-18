# Changelog

All notable changes to `leafr-timelapse-generator` are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [1.1.1] - 2026-10-18

### Added

- Log process RSS and Go heap statistics before and after returning unused Go heap memory to the OS at the end of each conversion.

## [1.1.0]

### Added

- `reading` status: the service is reading the sequence file and checking the pictures. It is the first stage after `queued`.
- `saving` status: the finished files are moved to their final names and the sequence file is deleted if `deleteSequenceFile` is set. It comes after `thumbnail` (or `encoding` without a thumbnail) and before `completed`.
- Log lines for the new stages: 📖 for `reading`, 💾 for `saving`. See [README.md](../README.md#logging).

### Changed

- `preparing` now only creates the output folders and writes the list ffmpeg reads. Reading the sequence file moved to `reading`.
- The `started` WebSocket event carries `status` `reading` instead of `preparing`.
- README: the job status table, the `started` event, the one-conversion-at-a-time rule and the log table list the new statuses.

### Migration

For consumers that switch on `status` or on the `started` event:

| Before | After |
|--------|-------|
| `started` has `status` `preparing` | `started` has `status` `reading`. |
| `preparing` means reading the sequence file | `reading` means reading the sequence file. `preparing` is now short. |
| A conversion is active in `queued`, `preparing`, `encoding` or `thumbnail` | Also `reading` and `saving`. |
| `completed` follows `thumbnail` or `encoding` | `saving` may come in between. |

Treat every status other than `completed`, `failed` and `cancelled` as active. Do not depend on the exact order of the non-terminal statuses.
