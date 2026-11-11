# Voice Timeline Renderer

## Service goal

Voice Timeline Renderer is a backend service for assembling narrated PCM audio
into a deterministic mixdown. It gives audio-platform engineers a small HTTP
surface for registering 16-bit WAV sources, composing ordered timeline spans,
and rendering a normalized output along with a machine-readable render
receipt.

The project is intentionally production-only. Automated tests, fixtures, and a
test command are deferred to the downstream task stage, which will add focused
unit, integration, and end-to-end verification.

## Users

- Audio-platform engineers who need a self-hosted assembly stage.
- Audio editing tooling that submits WAV sources and timeline operations.
- Quality engineers who replay a render and compare the receipt with the output.

## Project shape and stack

- Shape: backend.
- Language: Go.
- Runtime: Go bundled HTTP packages.
- Persistence: a local data directory containing metadata JSON and media files.
- External services: none.

## Core entities

### Source asset

A source asset is an immutable uploaded PCM WAV. Its identity, SHA-256 digest,
frame count, sample rate, channel count, peak amplitude, RMS amplitude, and duration are
recorded after decoding. A source is never updated in place.

Invariants:

- The payload must be a RIFF/WAVE file with 16-bit integer PCM samples.
- Only mono and stereo layouts are accepted.
- The digest and signal measurements describe the exact bytes stored.
- Decoded payloads are bounded by configured byte and duration limits.

### Assembly timeline

A timeline is an ordered set of spans over existing source assets. A span
selects a half-open source frame interval, a gain in decibels, and an optional
crossfade with the preceding span.

Invariants:

- A timeline has at least one span.
- Every referenced source exists.
- Source intervals are non-empty and remain inside the source frame count.
- The first span has no crossfade.
- A crossfade cannot exceed the shorter neighboring span.
- Output channel count is explicit and independent from an input source's
  channel count.

### Render job

A render job is the durable record of one timeline execution.

State machine:

`queued -> rendering -> finalizing -> completed`

Any active state may transition to `failed`. `completed` and `failed` are
terminal. A failed job stores a stable error code and message. A completed job
stores the output file, output metadata, and immutable render receipt.

A completed job also carries a durable retention record: the output expiry
stamped at completion, and later the reclamation time and reason once the
output artifact leaves storage. Reclamation is not a state transition; the
terminal state and its timestamps are preserved.

### Render receipt

A receipt records source identifiers, span placement, gain adjustments, peak
and RMS measurements, final normalization gain, frame count, output digest, and
non-fatal warnings. It is written only after the output bytes have been
atomically committed. The receipt document is retained after output
reclamation, so the delivery history outlives the downloadable artifact.

## Workflows

### 1. Register audio source

Entry point: `PUT /v1/sources/{source_id}`.

1. The HTTP boundary enforces request size and base64 decoding.
2. The service validates identity fields and decodes the WAV structure.
3. The codec layer verifies PCM format, frame alignment, and signal bounds.
4. Signal measurements are computed from the decoded samples.
5. Storage writes the media bytes and metadata using an atomic commit.

Success returns source metadata. Failure returns a stable validation error and
leaves no visible partial source.

### 2. Compose assembly timeline

Entry point: `PUT /v1/timelines/{timeline_id}`.

1. The HTTP boundary decodes and sizes the span list.
2. Domain validation checks ranges, gain limits, crossfade limits, and temporal
   ordering.
3. The service resolves every source reference from storage.
4. A timeline record is persisted once all spans are valid.

Success returns a ready timeline. Failure reports the first stable validation
issue without persisting a partial timeline.

### 3. Render timeline mixdown

Entry point: `PUT /v1/renders/{render_id}` with a `timeline_id` body field.

1. The service creates a durable `queued` job.
2. The renderer moves to `rendering` and loads all referenced sources.
3. Each source interval is resampled to the timeline sample rate, mapped to the
   requested channel layout, gain-adjusted, faded, and placed on the timeline.
4. Overlapping neighboring spans are mixed with equal-power crossfade weights.
5. The finalizing stage applies peak normalization to the requested target,
   writes a WAV and receipt through temporary files, and renames them into
   place atomically.
6. The job moves to `completed`. Any error records a failed terminal state and
   removes unpublished temporary output.

Success is observable through the job resource, the downloaded WAV, and the
receipt. Failure is observable through the terminal job error code.

### 4. Reclaim expired render output

Entry point: background retention sweep (startup plus configured interval).

1. The sweep lists every completed job and inspects its output artifact.
2. An output past its persisted expiry is reclaimed: the job's retention
   record is updated first, then the output file is deleted. The receipt and
   job records remain as the delivery history.
3. A job already marked reclaimed whose file still exists (the process exited
   between the two steps) has the deletion finalized.
4. A completed job whose output file vanished outside the retention flow is
   reconciled to the reclaimed state with reason `output_missing`, so the job
   stops claiming the output is downloadable.
5. Legacy completed jobs without a retention record are stamped with an
   expiry derived from their completion time.

A download admitted before reclamation reads the job and the output bytes
under one storage lock and always delivers the complete original file.
Downloads started after reclamation receive the stable
`render_output_reclaimed` error, and new downloads cannot extend an expired
artifact's lifetime.

## HTTP interface

- `GET /healthz`
- `PUT /v1/sources/{source_id}`
- `GET /v1/sources/{id}`
- `PUT /v1/timelines/{timeline_id}`
- `GET /v1/timelines/{id}`
- `PUT /v1/renders/{render_id}`
- `GET /v1/renders/{id}`
- `GET /v1/renders/{id}/receipt`
- `GET /v1/renders/{id}/output`

The receipt route keeps serving the receipt after reclamation, annotated with
the job's retention record. The output route answers `410` with the stable
`render_output_reclaimed` code once the artifact has been reclaimed.

Errors use a JSON envelope:

```json
{
  "error": {
    "code": "invalid_source_interval",
    "message": "source interval ends beyond the source frame count",
    "field": "spans[0].source_end_frame"
  }
}
```

## Modules

| Module | Responsibility |
|---|---|
| `httpapi` | HTTP routing, request limits, JSON encoding, media responses |
| `service` | Workflow orchestration and job lifecycle |
| `domain` | Entities, validation rules, stable errors, state transitions |
| `audiocodec` | WAV parsing and encoding, resampling, channel mapping, signal metrics |
| `render` | Span evaluation, crossfade mixing, normalization, receipt construction |
| `storage` | Atomic source, timeline, job, output, and receipt persistence |

## Engineering mechanisms

- State transitions: render jobs persist every active and terminal transition.
- Error propagation: domain errors retain stable codes through HTTP responses.
- Resource lifecycle: temporary files are cleaned after failed publication;
  completed output is reclaimed when its persisted retention period ends.
- Retention consistency: the reclamation record is persisted before the output
  file is deleted, downloads assemble the file under one storage read lock,
  and a startup sweep reconciles crash windows and externally deleted files,
  so the job, receipt, and output endpoints always agree on availability.
- Concurrency: storage guards indexes and registry updates; unique IDs and
  atomic renames prevent duplicate or half-visible records.
- Validation boundaries: request size, PCM layout, timeline bounds, and gain
  ranges are checked before allocation-heavy work.

## Validation plan

The runtime manifest exposes one bounded production smoke path per workflow:

- Register a generated WAV, read it back, and confirm a malformed payload fails.
- Compose a two-span timeline, read it back, and confirm an invalid crossfade
  fails.
- Render a crossfaded timeline, download the output, decode it, and match core
  receipt measurements.
- Render with a bounded retention period, sweep at a synthetic expiry instant,
  and confirm the job, receipt, and output endpoints consistently report the
  reclamation across concurrent downloads, a service restart, and a manually
  deleted output file.

These checks use the real HTTP entry points and local storage. They are not a
replacement for the test suite that the later task stage will add.
