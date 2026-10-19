# Changelog

All notable changes to `leafr-timelapse-generator` are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [1.2.0] - 2026-10-19

### Added

- Redis as the shared backend for running several replicas: job state, the job queue and the progress events live in Redis instead of process memory.
  - The queue is a Redis stream (`<prefix>:queue`) read through the consumer group `workers`. Each replica runs one conversion at a time and acknowledges the entry once the job is `completed`, `failed` or `cancelled`.
  - Job state is stored in `<prefix>:job:<id>` (expires after `JOB_RETENTION_MINUTES`) and indexed in `<prefix>:jobs`, so `GET /api/timelapses` and `GET /api/timelapses/{id}` give the same answer on every replica.
  - Progress and every other job event are published on the pub/sub channel `<prefix>:events` (not queued). Each replica forwards them to its own WebSocket clients, so a client sees the jobs of all replicas. If a replica loses its Redis subscription, its WebSocket clients are disconnected (close code `1013`) and resynchronise from the snapshot when they reconnect.
  - Cancellation works on any replica: queued jobs are cancelled directly in Redis, running jobs through the pub/sub channel `<prefix>:cancel`.
  - Failover: the replica running a job renews its queue entry every `JOB_CLAIM_IDLE_SECONDS / 3`. If it dies, another replica takes the entry over after `JOB_CLAIM_IDLE_SECONDS`; a job that had not started runs normally, a running one ends `failed` with the new error code `worker_lost`.
- Environment variables: `REDIS_ADDR` (default `127.0.0.1:6379`), `REDIS_PASSWORD`, `REDIS_DB` (default `0`), `REDIS_KEY_PREFIX` (default `leafr:timelapse`) and `JOB_CLAIM_IDLE_SECONDS` (default `60`, minimum `5`). See [README.md](../README.md#configuration).
- `activeJobs` in the WebSocket `snapshot`: all queued and running conversions, oldest first.
- `409 output_in_use`: another queued or running job writes the same output or thumbnail file.
- `503 store_unavailable`: Redis cannot be reached.
- On startup the service waits up to 30 s for Redis and exits if it does not answer.

### Changed

- **Breaking:** conversions are queued. `POST /api/timelapses` no longer returns `409 conversion_in_progress` while another conversion is active; the job stays `queued` until a replica is free. Several jobs can be `queued` or running at the same time (one per replica).
- `active` in the `snapshot` is now the oldest queued or running conversion (use `activeJobs` for all of them). It no longer means "the conversion that blocks new requests".
- Jobs waiting in the queue survive a restart or shutdown of a replica. Only the job a replica is running when it is shut down ends `cancelled` with `service_shutdown`.
- Finished jobs and the job history survive a restart of the service (they are lost only if Redis loses its data).
- `GET /healthz` is still a plain liveness check; it does not check Redis.

### Removed

- The `409 conversion_in_progress` error and the one-conversion-at-a-time rule.

### Docker Compose

- New `leafr-redis` service (`redis:8.10.1-alpine3.23`, host network, `mem_limit: 1g`, no persistence: `--save "" --appendonly no`, health check `redis-cli --raw incr ping`, `json-file` logs limited to 2 x 16 MB).
- `leafr-timelapse-generator` now has `REDIS_ADDR: host.docker.internal:6379` and `REDIS_KEY_PREFIX: leafr:timelapse`, `extra_hosts: host.docker.internal:host-gateway` to reach the host-network Redis, and `depends_on: leafr-redis` with `condition: service_healthy`.
- Replicas (several containers or hosts) must use the same media directory mount and the same Redis settings. Each replica needs its own published port (or `LISTEN_ADDR`); the compose file runs one replica.

### Migration

| Before | After |
|--------|-------|
| `POST` while a conversion is active: `409 conversion_in_progress` | `202`; the job is `queued` until a replica is free. Remove any handling that relies on the `409`. |
| At most one active job; `active` in the snapshot blocks new requests | Any number of active jobs; read `activeJobs`. `active` is only the oldest one. |
| A shutdown cancels the queued job as well | The queued job stays in the queue. Only the running job is cancelled (`service_shutdown`). |
| A crashed service loses all jobs | A job running on a crashed replica ends `failed` with `worker_lost`; resubmit it. |
| No external dependency | Redis is required. Start it before (or with) the service and set `REDIS_ADDR`. |

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
