# ConvertFlow — Design Spec

Date: 2026-10-13
Status: Draft for review

## 1. Purpose and scope

ConvertFlow is a web application for transcoding video and audio files by drag-and-drop, with a REST API for automation. It runs on-premises in an air-gapped network, is built exclusively on FOSS, uses a dark UI, and has **no authentication** (explicit decision of the operator).

Target use: medium load on a single host (several concurrent jobs, backlog queue, files up to several GB). CPU-only transcoding by default; hardware acceleration is optional and detected at runtime.

### Grundschutz position
IT-Grundschutz (BSI) certifies an organization's ISMS, not software. ConvertFlow therefore:
- implements the technical requirements of the applicable Bausteine (see section 9),
- ships documentation that feeds the operator's Strukturanalyse / Modellierung / audit,
- does **not** claim "full compliance" or certification.

### Decision: no authentication
Conflicts with ORP.4 and parts of CON.10/APP.3.1. Handled as a documented, formally accepted risk with compensating controls (bind-address setting, network isolation, hardened containers, audit log) and a risk-acceptance template for the operator's ISB. No optional auth mechanism is built in this scope.

### Non-goals
- Authentication, users, roles, multi-tenancy.
- Multi-node clusters / distributed storage.
- Playback, streaming/HLS, editing, subtitles burn-in workflows.
- Verifying users' rights to the content they convert (operator responsibility).
- Producing the Grundschutz certification itself.

## 2. Architecture

```
Browser / automation client
        │ HTTP (UI, REST, SSE)
        ▼
 ┌──────────────┐     ┌────────────┐
 │ convertflow  │────▶│  postgres  │  jobs, presets, worker capabilities, audit events
 │  api + ui    │     └─────▲──────┘
 └──────┬───────┘           │ claim jobs (FOR UPDATE SKIP LOCKED)
        │ shared volume     │
        ▼                   │
   /data/in  /data/out  ◀── worker (×N): ffprobe → ffmpeg
```

- Language: Go. One codebase, two images: `convertflow-api` (no FFmpeg, no media decoder on the network-facing side) and `convertflow-worker` (contains FFmpeg/ffprobe).
- Queue: Postgres (`SELECT … FOR UPDATE SKIP LOCKED`). No Redis.
- Workers scale with `--scale worker=N`.
- Frontend: TypeScript, small bundled framework, embedded into the API binary via `go:embed`. No CDN, web fonts, or telemetry.
- Shared volume for input/output files; Postgres for all state.

## 3. Deployment and hardening

- Delivered as OCI images plus `compose.yaml` (Docker and Podman compatible). Images are built outside the air gap and imported with `docker load` / `podman load`.
- All containers: non-root, read-only root filesystem (tmpfs for scratch), all capabilities dropped, `no-new-privileges`, default seccomp profile, CPU/memory/pids limits, pinned image digests.
- Worker network is internal with no egress; FFmpeg runs without network access.
- API bind address and port configurable (default: localhost-only in the example compose; operator opens it deliberately).
- Optional TLS 1.2+ with operator-supplied certificate, or operation behind the operator's reverse proxy.
- Each release ships: SBOM (CycloneDX), checksums, signed images, `THIRD-PARTY-LICENSES`.

## 4. FFmpeg flavors and licensing

Build argument `FFMPEG_FLAVOR`:
- `lgpl` (**default**): royalty-free codecs only.
  - Video: AV1 (SVT-AV1, libaom), VP9, VP8.
  - Audio: Opus, Vorbis, FLAC, PCM/WAV, MP3 (LAME).
  - Containers: WebM, MKV, Ogg, MP4.
  - Decoding of other input formats via FFmpeg native decoders.
- `full` (opt-in): adds x264, x265, AAC. GPL build; prints a licensing and patent notice at build time. Patent licensing (H.264/AAC/HEVC pools) is the operator's responsibility.

Rules:
- ConvertFlow code: Apache-2.0 (or MIT). FFmpeg is invoked as an external process, keeping our code separate from GPL components.
- Go dependencies: permissive only (MIT/BSD/Apache); CI license check fails on AGPL/GPL.
- Images bundle FFmpeg license texts and a source offer. If a `full` image is ever given to a third party, GPL source obligations apply.
- Documented in `docs/licensing.md`; not legal advice.

## 5. Capability detection and presets

- On startup each worker probes `ffmpeg -encoders`, CPU instruction sets (e.g. AVX2), and passed-through hardware devices (VAAPI/QSV/NVENC), then writes its capabilities to Postgres and refreshes them via heartbeat.
- `GET /api/v1/presets` returns only presets runnable by at least one live worker.
- Each preset defines a **fallback chain** (e.g. "Modern video": AV1 hardware → SVT-AV1 software → VP9). The job records the encoder actually used; the UI displays it.
- Requesting an unavailable codec yields `422` problem+json at submission, not a late failure.
- Software AV1 is CPU-heavy: presets expose a speed/quality setting (SVT-AV1 preset 4–12), default biased to speed.
- The host does not need AV1 support for software encoding; the encoders are bundled in the worker image. Hardware AV1 is optional and only used if detected.
- UI notes that not all players decode AV1; VP9 and MP4 presets remain available.
- Hardware acceleration passthrough is a documented optional compose override, not part of the default setup.

## 6. API

OpenAPI 3.1 served at `/api/openapi.json`, with an offline viewer. Errors use RFC 9457 problem+json.

| Endpoint | Purpose |
|---|---|
| `POST /api/v1/jobs` | Streamed/multipart upload plus `preset` or custom `options`. Returns `202` + job ID. |
| `GET /api/v1/jobs`, `GET /api/v1/jobs/{id}` | Status, progress, ffprobe metadata, encoder used, error summary. |
| `GET /api/v1/jobs/{id}/events` | SSE progress stream. |
| `GET /api/v1/jobs/{id}/output` | Download result (range requests supported). |
| `DELETE /api/v1/jobs/{id}` | Idle job (queued, done, failed, canceled): delete record and files (`204`). Probing/running job: the first `DELETE` only requests cancellation (`202`); the files are removed when the job reaches a terminal state, and a second `DELETE` removes the record. |
| `GET /api/v1/presets` | Presets runnable on the current workers. |
| `GET /healthz`, `GET /metrics` | Liveness; Prometheus metrics. |

## 7. Job lifecycle

States: `queued → probing → running → done`, or `failed` / `canceled`.

1. Upload streams to `/data/in/<id>` (never buffered in memory); size limit configurable.
2. A worker claims the job (`SKIP LOCKED`) and runs `ffprobe` to confirm the file is media and within limits (duration, dimensions, stream count).
3. FFmpeg arguments are built from a **fixed allow-list** of options; user input is never passed to a shell or interpolated as free-form arguments.
4. Progress is read from FFmpeg `-progress` and persisted.
5. Output goes to `/data/out/<id>`.
6. A janitor removes files and metadata after the configurable retention period (default 24 h). Deletion is explicit on `DELETE`.
7. Worker crash detection via heartbeat; job requeued up to N times, then `failed`.
8. Failed jobs keep a sanitized FFmpeg error summary (no filesystem paths leaked).

## 8. UI

- Single dark-themed page (dark only), WCAG AA contrast, keyboard accessible.
- Drag-and-drop zone accepting multiple files, preset dropdown, "advanced" options panel.
- Live job table: progress bars (via SSE), encoder used, download and cancel actions.
- Fully self-contained assets; works with no outbound connectivity.

## 9. Grundschutz mapping

| Baustein | Implementation |
|---|---|
| CON.10 / APP.3.1 Web applications | Input validation, CSP without inline scripts, security headers, upload size/type limits, no shell injection surface. |
| APP.3.2 Web server | TLS 1.2+ capable or reverse-proxy ready; minimal exposed surface. |
| CON.8 Software development | SBOM, dependency and license scanning, signed images, reproducible builds, documented secure development process. |
| OPS.1.1.5 Logging | Structured JSON audit log to stdout (job created/started/finished/failed/deleted, client IP; never file contents); Prometheus metrics. |
| CON.6 Deletion | Retention janitor; removal of files and metadata. |
| CON.3 Backup | Documents that only the Postgres dump matters; media is ephemeral. |
| SYS.1.6 Containerization | Hardening per section 3. |
| ORP.4 Identity management | Not implemented (operator decision); accepted-risk template and compensating controls. |

## 10. Testing

- Unit: option allow-list, FFmpeg argument builder (including injection attempts), preset fallback resolution.
- Integration: real FFmpeg on small generated clips — upload, convert, SSE, download, cancel, retention, worker-crash requeue.
- Hostile input: truncated files, wrong extensions, oversized dimensions/duration, decompression-bomb-like media.
- Container tests: assert non-root, read-only rootfs, dropped capabilities; build fails on regression.
- CI security: Trivy/Grype image scan, govulncheck, gosec, license check.

## 11. Documentation deliverables

1. `README` — air-gapped quick start (load, up).
2. Operations guide — config, TLS, upgrades, backup, monitoring.
3. Grundschutz mapping — section 9 expanded per requirement.
4. Risk-acceptance template for missing authentication (for the operator's ISB).
5. Licensing document — FFmpeg flavors, patent notice, third-party licenses.

## 12. Open assumptions

- Default FFmpeg flavor is `lgpl`; `full` is opt-in.
- CPU-only by default; hardware acceleration optional.
- Retention default 24 h, size/duration limits configurable with sensible defaults.
- Project license Apache-2.0 unless the operator prefers otherwise.
