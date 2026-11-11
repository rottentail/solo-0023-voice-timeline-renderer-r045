# Voice Timeline Renderer

Voice Timeline Renderer is a self-hosted Go service for turning validated PCM
WAV recordings into a crossfaded, peak-normalized mixdown. The service exposes
an idempotent HTTP surface, keeps media and metadata under one data directory,
and records every render transition.

The implementation covers:

- 16-bit integer PCM WAV validation and signal measurement.
- Ordered timeline spans with source-frame selection, gain, and crossfade.
- Linear sample-rate conversion and mono/stereo channel mapping.
- Equal-power neighboring-span crossfades.
- Peak normalization into a requested dBFS target.
- Durable render states, receipts, and atomically committed output folders.
- Retention-driven reclamation of completed render output with durable
  reclamation records.

Automated tests are intentionally deferred. A later task stage will add unit,
integration, and end-to-end verification around the production interfaces.

## Requirements

- Go 1.23 or newer.
- A writable local directory for source media, timeline metadata, render jobs,
  output WAV files, and receipts.

No network service, database server, or third-party Go module is required.

## Build

```bash
go build ./...
```

## Run

```bash
go run ./cmd/voiceservice \
  -addr :8093 \
  -data ./var/voice-timeline
```

The health endpoint is `GET http://127.0.0.1:8093/healthz`.

Runtime settings are supplied through arguments:

- `-addr`: HTTP listen address, default `:8093`.
- `-data`: persistent data root, default `./var/voice-timeline`.
- `-render-retention`: how long a completed render output stays downloadable
  before the retention sweep reclaims it, default `168h` (7 days).
  Non-positive values fall back to the default; pick a very large duration to
  keep outputs effectively forever.
- `-render-retention-sweep`: interval between retention sweeps, default `1m`.
  `0` disables the background sweeper (startup reconciliation is skipped too).

No environment variable is required. Do not put credentials in the repository;
the service does not consume any remote credential.

## HTTP interface

### Register a source

`PUT /v1/sources/{source_id}`

```json
{
  "name": "opening take",
  "wav_base64": "UklGR..."
}
```

The source ID is chosen by the caller and must be 3-64 characters using
letters, digits, underscore, or hyphen. Repeating the same ID with exactly the
same name and bytes is idempotent. Reusing it with different content returns
the `source_conflict` error.

Response:

```json
{
  "id": "opening_take_01",
  "name": "opening take",
  "digest": "b7c0...",
  "byte_count": 96044,
  "sample_rate": 48000,
  "channels": 1,
  "bits_per_sample": 16,
  "frame_count": 48000,
  "duration_ms": 1000,
  "peak_dbfs": -8.0,
  "rms_dbfs": -11.0,
  "created_at": "2026-11-09T12:00:00Z"
}
```

### Compose a timeline

`PUT /v1/timelines/{timeline_id}`

```json
{
  "name": "two part narration",
  "sample_rate": 48000,
  "output_channels": 2,
  "target_peak_dbfs": -1,
  "spans": [
    {
      "source_id": "opening_take_01",
      "source_start_frame": 0,
      "source_end_frame": 48000,
      "gain_db": -2,
      "crossfade_frames": 0
    },
    {
      "source_id": "closing_take_01",
      "source_start_frame": 0,
      "source_end_frame": 44100,
      "gain_db": 0,
      "crossfade_frames": 4800
    }
  ]
}
```

Spans are sequential. A later span overlaps its predecessor by
`crossfade_frames`; the overlap is mixed with equal-power weights. A crossfade
must fit within both neighboring spans.

### Start a render

`PUT /v1/renders/{render_id}`

```json
{
  "timeline_id": "two_part_narration_01"
}
```

The render runs through the durable states
`queued -> rendering -> finalizing -> completed`. A failed render records a
terminal `failed` state with a stable error code and does not expose a partial
output folder.

### Read resources

- `GET /v1/sources/{source_id}`
- `GET /v1/timelines/{timeline_id}`
- `GET /v1/renders/{render_id}`
- `GET /v1/renders/{render_id}/receipt`
- `GET /v1/renders/{render_id}/output`

The output route returns `audio/wav`. The receipt records source digests, span
placement, channel mapping, measured peak and RMS amplitude, normalization
gain, output digest, and non-fatal warnings.

## Render retention and reclamation

Completed render output is kept for `-render-retention` after completion. The
expiry is persisted on the job record when the render completes, so restarts
never reset the clock and downloads never extend it. A background sweep (plus
one reconciliation pass at startup) reclaims expired artifacts:

- Reclamation deletes only `renders/{render_id}/output.wav`. The job record
  and `receipt.json` stay behind as the durable history of what was
  delivered, and the job's `retention` block records `expires_at`,
  `reclaimed_at`, and `reclaim_reason`.
- The reclamation fact is persisted before the file is deleted, so the job,
  receipt, and output endpoints never disagree — not even if the process
  exits between the two steps. A leftover file is finished by the next sweep.
- A download admitted before reclamation assembles the complete original
  bytes under one storage lock and is never truncated. Downloads started
  after reclamation receive `410` with the stable `render_output_reclaimed`
  error, and fresh downloads cannot pin an expired artifact in storage.
- After reclamation, `GET /v1/renders/{id}` still returns the completed job
  (with the retention record), `GET /v1/renders/{id}/receipt` still returns
  the receipt annotated with the same retention record, and only the output
  download is gone.
- If the output file disappears outside the retention flow (for example an
  operator deletes the directory to free space), the next sweep reconciles
  the job to the same reclaimed state with reason `output_missing` instead of
  letting it claim the output is downloadable.
- Jobs completed before retention existed are stamped with an expiry derived
  from their completion time on the first sweep, so pre-existing output piles
  are reclaimed as their retention period elapses.

## Data directory

```text
var/voice-timeline/
  sources/<source_id>/
    source.wav
    metadata.json
  timelines/<timeline_id>.json
  jobs/<render_id>.json
  renders/<render_id>/
    output.wav        (deleted when the retention period ends)
    receipt.json      (kept as the durable delivery history)
  tmp/
```

Source and render folders are assembled in `tmp/` and moved into place with one
directory rename. Metadata updates use same-directory temporary files followed
by rename.

## Source layout

```text
cmd/voiceservice/       service entry point
cmd/timelinecheck/      workflow execution entry point
internal/httpapi/       routing, request limits, JSON errors, media response
internal/service/       workflow orchestration and idempotency
internal/domain/        entities, validation, errors, and render transitions
internal/audiocodec/    WAV handling, transforms, and signal measurement
internal/render/        timeline preparation, mixing, normalization, receipt
internal/storage/       atomic local repository
internal/checkflow/     bounded production workflow checks
```

## Workflow checks

Run each declared production check from the repository root:

```bash
go run ./cmd/timelinecheck -workflow ingest-audio-source
go run ./cmd/timelinecheck -workflow compose-timeline
go run ./cmd/timelinecheck -workflow render-mixdown
go run ./cmd/timelinecheck -workflow render-retention
```

Each check creates an isolated temporary data directory, starts the real HTTP
handler, sends generated WAV inputs, and verifies read-back or rendered output.
The retention check additionally drives the sweep at synthetic timestamps,
races downloads against reclamation, restarts the service over the same data
directory, and reconciles a manually deleted output file. The checks are
bounded and do not require network access.

## Deferred test boundary

This baseline contains no unit tests, test fixtures, browser checks, or
`test_command`. The downstream task stage owns test creation and red/green
verification. It should extend coverage around WAV chunk edge cases, timeline
crossfade bounds, failed job cleanup, concurrent idempotency, and HTTP error
mapping.
