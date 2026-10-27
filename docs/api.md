# REST API guide

The machine-readable source of truth is the OpenAPI 3.1 document served at `GET /api/openapi.json` (the UI renders it at `/docs.html`). This page is a short guide for automation. The upload, status, SSE, download, delete, presets and metrics examples were run with curl against the podman stack (rootless podman 6.1.2); the Range request is covered by the Go test `TestOutputOverRealServerLargeAndRange` and was not run with curl. The direct-upload examples (S3 storage) were run with curl against the `compose.s3.yaml` profile (rootless podman 6.1.2, MinIO). `BASE` is the service URL.

```bash
BASE=http://127.0.0.1:8080          # or https://... when TLS is enabled (curl --cacert ca.pem)
```

There is **no authentication**. State-changing requests (`POST`, `DELETE`) that a browser marks as cross-origin are refused with `403`; automation that sends no `Origin`/`Sec-Fetch-Site` header (curl, scripts, HTTP libraries) is unaffected, but do not set an `Origin` header that differs from the `Host` you call. Do not put secrets into filenames or options; restrict network access as described in [security.md](security.md).

## Endpoints

| Method and path | Purpose | Success | Notes |
|---|---|---|---|
| `POST /api/v1/jobs` | Upload a file and create a job | `202` + job JSON, `Location` header | multipart/form-data, see below. S3 storage also accepts JSON and answers `201` with a presigned upload form, see [Direct upload](#direct-upload-s3-storage) |
| `POST /api/v1/jobs/{id}/complete` | S3 storage only: queue a job after its direct upload (JSON `POST /api/v1/jobs`) | `200` + job JSON | `409` nothing uploaded, size mismatch or expired; `404` with local storage |
| `GET /api/v1/jobs` | Most recent jobs, newest first, at most 200 | `200` | JSON array |
| `GET /api/v1/jobs/{id}` | Status, progress, metadata | `200` | `404` for unknown or malformed ids |
| `DELETE /api/v1/jobs/{id}` | Delete an idle job and its files, or request cancellation of an active one | `204` or `202` | see "Cancel and delete" |
| `GET /api/v1/jobs/{id}/events` | Server-Sent Events progress stream | `200` | `503` when the stream limit is reached |
| `GET /api/v1/jobs/{id}/output` | Download the result | `200` / `206` (S3 storage: `302` to a presigned URL) | Range requests supported; `409` not ready, `410` removed |
| `GET /api/v1/presets` | Presets runnable on the currently live workers | `200` | only presets some live worker can encode (after the flavor policy); the `ConvertFlow-Upload-Mode` header says how the UI uploads |
| `GET /api/openapi.json` | The OpenAPI document | `200` | |
| `GET /healthz` | Liveness with a short database ping | `200` body `ok` | `503` when the database is unreachable |
| `GET /metrics` | Prometheus metrics | `200` | see [operations.md](operations.md#monitoring) |

## Presets

```bash
curl -s $BASE/api/v1/presets
# [{"id":"video-av1","label":"Video – AV1 + Opus (WebM)","kind":"video","ext":"webm"}, {"id":"audio-flac",...}, ...]
```

Preset ids: `video-av1`, `video-vp9`, `audio-opus`, `audio-vorbis`, `audio-flac`, `audio-mp3`, `audio-wav` (all flavors) and `video-h264`, `video-h265`, `audio-aac` (only with the opt-in `full` flavor, see [licensing.md](licensing.md)). The list returned by `/api/v1/presets` is authoritative: a preset appears only while at least one worker that can run it has been seen in the last 30 seconds. `video-av1` falls back to VP9 when no AV1 encoder is available. Video presets need a video stream in the input, audio presets need an audio stream; otherwise the job fails with a short message. Only the first video and first audio stream are converted; subtitles, data streams, chapters and metadata are dropped.

## Upload

`POST /api/v1/jobs` takes `multipart/form-data` with these fields:

| Field | Required | Meaning |
|---|---|---|
| `preset` | one of `preset` or `options` | a preset id from `/api/v1/presets` |
| `options` | one of `preset` or `options` | a single JSON object with known fields only (below) |
| `file` | yes | the media file |

**Field order matters: send `preset` (or `options`) before `file`.** The server streams the file to disk and validates the options first so that an invalid request is rejected before any bytes are stored. Exactly one `preset` or `options` field, once; unknown field names are rejected with `400`; anything after the `file` part is ignored. With curl, `-F` fields are sent in command-line order:

```bash
curl -s -F preset=audio-flac -F file=@song.wav $BASE/api/v1/jobs
```

```json
{"id":"9eb8f284-cc78-40b8-8661-39c72f3f065e","status":"queued","filename":"song.wav","preset":"audio-flac","progress":0,"encoder_used":"","attempts":0,"created_at":"2026-10-17T19:32:43.456003Z","updated_at":"2026-10-17T19:32:43.456003Z"}
```

### Options

Instead of `preset=...`, send a JSON object (all fields except `preset` optional; unknown fields are rejected with `422`):

```bash
curl -s -F 'options={"preset":"video-vp9","crf":36,"max_height":720,"speed":8}' -F file=@clip.mkv $BASE/api/v1/jobs
```

| Field | Values | Meaning |
|---|---|---|
| `preset` | preset id | required |
| `speed` | integer 0-10 | 0 best quality (slowest), 10 fastest; default 6; ignored by audio presets |
| `crf` | integer 18-45 | video quality (lower is better); defaults per encoder; ignored by audio presets |
| `max_height` | 240, 360, 480, 720, 1080, 1440, 2160 | downscale to at most this height, aspect ratio kept; never upscales |
| `audio_bitrate_k` | 64, 96, 128, 192, 256, 320 | audio bitrate in kbit/s for lossy audio encoders |

These are the only user-controlled parameters. They are validated against fixed lists and ranges; no free-form FFmpeg arguments exist.

### Job object

`id`, `status`, `filename` (sanitized display name), `preset`, `progress` (0-100), `encoder_used` (set once the job runs), `probe` (ffprobe summary, once probed), `error` (short sanitized message on failure), `attempts`, `created_at`, `updated_at`, `finished_at`, `output_url` (only when `done`).

Status values: `uploading` (S3 direct upload created, not completed yet), `queued`, `probing`, `running`, `done`, `failed`, `canceled`. `done`, `failed` and `canceled` are terminal.

## Direct upload (S3 storage)

With `CONVERTFLOW_STORAGE=s3` ([on-demand.md](on-demand.md)) a client can send the file straight to the bucket; the API never sees the bytes. The multipart upload above keeps working as well. `GET /api/v1/presets` answers `ConvertFlow-Upload-Mode: presigned` when browsers should use this path (`multipart` otherwise, including all local-storage deployments, where a JSON `POST` answers `400`):

```bash
curl -sI $BASE/api/v1/presets | grep -i convertflow-upload-mode
# Convertflow-Upload-Mode: presigned
```

### `POST /api/v1/jobs` with JSON

Body: exactly one of `preset` or `options` (the same object as the multipart `options` field), plus `filename` (display only) and `size` (bytes, required). At most 8 KiB, unknown fields are rejected.

```bash
size=$(stat -c %s song.wav)
curl -s -H 'Content-Type: application/json' \
  -d "{\"preset\":\"audio-flac\",\"filename\":\"song.wav\",\"size\":$size}" $BASE/api/v1/jobs > presign.json
jq . presign.json
```

```json
{
  "id": "58a2016e-a334-461c-9270-1e90e1890439",
  "upload": {
    "url": "http://127.0.0.1:18094/convertflow/",
    "fields": {
      "bucket": "convertflow",
      "key": "in/58a2016e-a334-461c-9270-1e90e1890439",
      "policy": "eyJleHBpcmF0aW9uIjoi...",
      "x-amz-algorithm": "AWS4-HMAC-SHA256",
      "x-amz-credential": "cfapiKwMRbVeysFj8Kps/20261026/us-east-1/s3/aws4_request",
      "x-amz-date": "20261026T204053Z",
      "x-amz-signature": "3015c6a7..."
    }
  },
  "expires_at": "2026-10-26T20:55:53Z"
}
```

The job now exists with status `uploading`. The answer is `201` with a `Location` header; the same checks as a multipart upload apply first (`503` queue full or too many uploads, `422` invalid options, `413` size over `CONVERTFLOW_MAX_UPLOAD_BYTES`, `400` missing or non-positive size). **The URL and fields are a credential** until `expires_at`: do not log or share them.

### Upload to the bucket

A `multipart/form-data` `POST` to `upload.url` with **every field of `upload.fields` first and the file last** (the store ignores fields after the file). Adding any other field makes the store refuse the form.

```bash
args=(); while IFS=$'\t' read -r k v; do args+=(--form-string "$k=$v"); done \
  < <(jq -r '.upload.fields | to_entries[] | [.key, .value] | @tsv' presign.json)
curl -s -w '%{http_code}\n' "${args[@]}" -F file=@song.wav "$(jq -r .upload.url presign.json)"
# 204
```

The store answers with its own XML errors, for example `400 EntityTooLarge` (over the limit), `400 EntityTooSmall` (empty) or `403 AccessDenied` (expired or altered form).

### `POST /api/v1/jobs/{id}/complete`

Queues the job after the upload. The API checks the stored object: present, 1 byte to the limit, and exactly the declared `size`.

```bash
id=$(jq -r .id presign.json)
curl -s -X POST $BASE/api/v1/jobs/$id/complete | jq -c '{id,status,filename}'
# {"id":"58a2016e-a334-461c-9270-1e90e1890439","status":"queued","filename":"song.wav"}
```

It is idempotent: calling it again answers `200` with the current job. `409` means nothing was uploaded yet, the size differs from the declared one (the object is then removed; upload again while the form is valid), or the upload expired; `413`/`400` a stored object over the limit or empty; `404` an unknown job or local storage.

```bash
curl -s -X POST $BASE/api/v1/jobs/$id/complete      # after uploading 176478 bytes for a declared size of 1000
# {"type":"about:blank","title":"Size mismatch","status":409,"detail":"The uploaded file does not have the announced size."}
```

### Download with S3 storage

`GET /api/v1/jobs/{id}/output` answers `302` to a presigned GET on the store, valid for at most 5 minutes, with `Cache-Control: no-store`. The file name and type are part of the signature. Follow the redirect (`curl -L`):

```bash
curl -s -o /dev/null -D - $BASE/api/v1/jobs/$id/output | grep -iE '^(HTTP|location|cache-control)'
# HTTP/1.1 302 Found
# Cache-Control: no-store
# Location: http://127.0.0.1:18094/convertflow/out/58a2016e-....1.flac?X-Amz-Algorithm=AWS4-HMAC-SHA256&...&response-content-disposition=attachment%3B%20filename%3D%22song.flac%22...
curl -fsL -OJ $BASE/api/v1/jobs/$id/output          # saves song.flac
```

## Status, progress and download

```bash
curl -s $BASE/api/v1/jobs/$ID | jq -c 'del(.probe)'
# {"id":"...","status":"done","filename":"song.wav","preset":"audio-flac","progress":100,"encoder_used":"flac","attempts":1,...,"output_url":"/api/v1/jobs/<id>/output"}

curl -OJ $BASE/api/v1/jobs/$ID/output          # saves song.flac (Content-Disposition: attachment; the extension follows the preset)
curl -s -H 'Range: bytes=0-1023' $BASE/api/v1/jobs/$ID/output -o part.bin    # 206
```

Poll `GET /api/v1/jobs/{id}` every second or two, or use SSE. `GET .../output` answers `409` while the job is not `done` and `410` once the result has been removed by retention.

### Server-Sent Events

```bash
curl -sN $BASE/api/v1/jobs/$ID/events
# retry: 3000
#
# data: {"id":"...","status":"running","progress":42.5,...}
```

Each `data:` line is the same job JSON as `GET /api/v1/jobs/{id}`; one is sent on every change. A `: keepalive` comment is sent every 15 s while nothing changes. The stream ends at a terminal status, when the job is deleted, after at most 10 minutes (reconnect; the browser `EventSource` does this using the advertised `retry: 3000`), or on server shutdown (`: shutdown` comment). At most `CONVERTFLOW_MAX_SSE_STREAMS` (default 64) streams are open at once; more get `503` with `Retry-After`. Disable buffering in a reverse proxy for this path (the server already sends `X-Accel-Buffering: no`).

## Cancel and delete

```bash
curl -s -X DELETE $BASE/api/v1/jobs/$ID -w '%{http_code}\n'
```

- Queued, done, failed or canceled job: removed together with its files, `204`.
- Job being probed or encoded (`probing`/`running`): **the first `DELETE` only cancels** - it requests cancellation and answers `202` with `{"id":"...","status":"canceling"}`; nothing is deleted yet. The worker stops FFmpeg, removes its partial output and its input file, and marks the job `canceled` (the files are gone once the job reaches that terminal state). Send a **second `DELETE`** after that to remove the job record (and any file that is still there, `204`). If the worker is gone, the janitor turns the job into `canceled` after `CONVERTFLOW_STALE_AFTER`. If it finished in the meantime, `409` and you delete it again.
- Unknown id: `404`.

## Errors (RFC 9457)

Errors are `application/problem+json`:

```json
{"type":"about:blank","title":"Bad request","status":400,"detail":"send the preset (or options) field before the file field"}
```

`detail` is always a fixed, client-safe message (no paths, SQL or internal errors). Unknown paths and wrong methods also answer with problem+json (`404`, `405` with an `Allow` header).

| Status | Meaning on `POST /api/v1/jobs` |
|---|---|
| `400` | not multipart, wrong field order, duplicate/unknown field, empty file, interrupted upload |
| `421` | the `Host` header is not `localhost`, an IP literal or listed in `CONVERTFLOW_ALLOWED_HOSTS` (DNS rebinding defense; applies to every route). Reaching the API by DNS name or through a proxy requires that variable |
| `403` | a browser marked the request as cross-origin (`Sec-Fetch-Site` not `same-origin`/`none`, or `Origin` differs from `Host`); also returned by `DELETE`. Clients that send neither header, such as curl and scripts, are not affected |
| `413` | file larger than `CONVERTFLOW_MAX_UPLOAD_BYTES` (default 8 GiB) |
| `422` | invalid options, or the preset is not available on any running worker (for example `video-h264` on the `lgpl` flavor) |
| `503` | too many concurrent uploads, or the job queue is full (`Retry-After: 30`) |
| `507` | not enough free space on the data volume |
| `500` | server fault; details are only in the server log |

Other endpoints: `404` unknown/malformed job id, `409` output not ready or job already finished, `410` output removed, `416` unsatisfiable range, `503` SSE limit or unhealthy database.

## Limits

| Limit | Default | Setting |
|---|---|---|
| Upload size | 8 GiB | `CONVERTFLOW_MAX_UPLOAD_BYTES` |
| Concurrent uploads | 4 | `CONVERTFLOW_MAX_CONCURRENT_UPLOADS` |
| Active jobs (queued + probing + running) | 100 | `CONVERTFLOW_MAX_QUEUED_JOBS` |
| Idle time during an upload | 60 s (fixed) | a stalled client is cut |
| Input duration / pixels / streams | 6 h / 35,389,440 px (8192x4320) / 16 | `CONVERTFLOW_MAX_DURATION_SEC`, `CONVERTFLOW_MAX_PIXELS`, `CONVERTFLOW_MAX_STREAMS` (checked by the worker after upload; violation fails the job) |
| Output size | 16 GiB | `CONVERTFLOW_MAX_OUTPUT_BYTES` |
| Run time of one conversion | 4 h | `CONVERTFLOW_JOB_TIMEOUT` |
| Retention of finished jobs and files | 24 h | `CONVERTFLOW_RETENTION` |

Details and valid ranges: [operations.md](operations.md#configuration-reference).

## Automation example

```bash
#!/usr/bin/env bash
set -euo pipefail
BASE=http://127.0.0.1:8080
id=$(curl -fs -F preset=audio-flac -F file=@"$1" $BASE/api/v1/jobs | jq -er .id)
while :; do
  st=$(curl -fs $BASE/api/v1/jobs/$id | jq -er .status)
  case $st in
    done) break ;;
    failed|canceled) curl -fs $BASE/api/v1/jobs/$id | jq -r .error >&2; exit 1 ;;
  esac
  sleep 2
done
curl -fOJ $BASE/api/v1/jobs/$id/output
curl -fs -X DELETE $BASE/api/v1/jobs/$id >/dev/null
```

`scripts/smoke.sh` in the repository does the same end to end against a throwaway stack.
