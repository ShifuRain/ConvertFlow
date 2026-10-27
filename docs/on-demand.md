# On-demand deployment and S3 storage

Status: storage on an S3-compatible bucket with direct browser uploads is implemented (`CONVERTFLOW_STORAGE=s3`). Workers that start per job and scale to zero, sessions and quotas, and Kubernetes manifests are later phases and are **not** implemented yet. Everything marked "verified" below was run with rootless podman 6.1.2, the docker-compose provider and MinIO `RELEASE.2026-10-23T20-06-51Z` (Chainguard rebuild). No AWS S3, Cloudflare R2 or Ceph endpoint was tested.

## Contents

- [Deployment shapes](#deployment-shapes)
- [How a direct upload works](#how-a-direct-upload-works)
- [Setting up a bucket](#setting-up-a-bucket)
- [The development and test profile (compose.s3.yaml)](#the-development-and-test-profile-composes3yaml)
- [Failure behavior](#failure-behavior)
- [Cost notes](#cost-notes)
- [Limits and open points](#limits-and-open-points)

## Deployment shapes

| | A. Single host (default) | B. On-demand (S3) |
|---|---|---|
| Processes | api + workers + PostgreSQL from `compose.yaml` | api (always on, small) + workers (later: started per job) + PostgreSQL |
| Storage | `CONVERTFLOW_STORAGE=local`: the shared `media` volume (`/data/in`, `/data/out`) | `CONVERTFLOW_STORAGE=s3`: one bucket, `in/<job id>` and `out/<job id>.<attempt>.<ext>` |
| Upload path | browser -> api -> volume | browser -> bucket (presigned POST); the API never sees the bytes |
| Download path | api streams the file | api answers `302` to a presigned GET; the bytes come from the bucket |
| Worker disk | the shared volume | a private scratch directory (tmpfs in compose) holding one input and one output at a time |
| Status | released (v0.1.0) | implemented; scale-to-zero not yet |

Shape A is unchanged and stays the default. The multipart upload through the API also keeps working in shape B (the bytes are streamed to the bucket), so curl users and scripts need no change.

## How a direct upload works

```
browser                         api                              bucket                    worker
   | GET /api/v1/presets         |                                  |                          |
   |---------------------------->| ConvertFlow-Upload-Mode: presigned                         |
   |<----------------------------| CSP connect-src/form-action += bucket origin                |
   | POST /api/v1/jobs (JSON:    |                                  |                          |
   |  preset|options, filename,  | checks queue cap, options, size  |                          |
   |  size)                      | job row: status "uploading"      |                          |
   |<----------------------------| 201 {id, upload:{url, fields}, expires_at}                  |
   | OPTIONS (CORS preflight, Origin = UI origin) ------------------>|                          |
   | POST multipart: the signed fields, then the file (last) ------->| checks the policy:       |
   |                             |                                  | key = in/<id>,           |
   |<---------------------------------------------------------------| 1..MAX bytes, not expired|
   |                         204 |                                  |                          |
   | POST /api/v1/jobs/{id}/complete                                |                          |
   |---------------------------->| HEAD in/<id>: size == declared,  |                          |
   |                             | 1..MAX -> status "queued"        |                          |
   |<----------------------------| 200 job                          |   claim, GET in/<id> --->|
   |                             |                                  |<-- PUT out/<id>.1.<ext> -|
   | GET /api/v1/jobs/{id}/output|                                  |                          |
   |---------------------------->| 302 Location: presigned GET (<= 5 min, no-store)            |
   | GET presigned URL ------------------------------------------------>| attachment; filename  |
```

- The upload form is valid for `CONVERTFLOW_S3_PRESIGN_TTL` (default 15 minutes). Its policy pins the bucket, the exact key `in/<id>`, a size of 1 byte to `CONVERTFLOW_MAX_UPLOAD_BYTES` and the expiry; any extra field (ACL, content type, metadata, redirect) makes the store refuse the form. The API's size check at `/complete` is the authority; the worker refuses an input over the limit again.
- The UI picks the path from the `ConvertFlow-Upload-Mode` header of `GET /api/v1/presets`. It cancels by aborting the request and deleting the job, deletes the job after any failure, maps store errors by their S3 code (for example `EntityTooLarge`), retries a busy store and `/complete` (which is idempotent), and keeps the existing concurrency limit and `503` backoff.
- A job left in `uploading` is failed by the janitor after the TTL plus one hour ("upload was not completed in time"); an object that arrives later is removed by the orphan sweep.

## Setting up a bucket

You need: a bucket that is **not public**, TLS between the browser and the store and between the api/worker and the store (anything other than localhost), two credentials (api and worker), a CORS rule and a lifecycle rule. `scripts/s3-bootstrap.sh` does all of this against any S3 endpoint the MinIO client can reach (it runs the pinned `mc` image; credentials go through stdin):

```bash
S3_ADMIN_URL=https://s3.example.org S3_ADMIN_USER_FILE=admin.key S3_ADMIN_PASSWORD_FILE=admin.secret \
CONVERTFLOW_S3_BUCKET=convertflow CONVERTFLOW_S3_REGION=eu-central-1 \
CONVERTFLOW_S3_CORS_ORIGINS=https://convert.example.org CONVERTFLOW_RETENTION=24h \
CONTAINER_RUNTIME=podman scripts/s3-bootstrap.sh
scripts/s3-bootstrap.sh --print-policies      # only print the policies below
```

Only the MinIO case was run; the commands for other providers are *unverified*.

**CORS.** Allowed origin: exactly the UI origin(s) (`scheme://host[:port]`, no wildcard), methods `POST` and `GET`, no extra request headers, `MaxAgeSeconds` 600. The UI sends no cookies to the store (`withCredentials` is off); MinIO and AWS still answer `Access-Control-Allow-Credentials: true`, which is harmless because the store sets no cookies and the form fields are the credential. **MinIO does not implement bucket CORS** (`NotImplemented`): set `MINIO_API_CORS_ALLOW_ORIGIN` to the UI origin(s) on the server instead; the script prints a warning in that case.

**Lifecycle (backstop).** `in/` expires after 2 days, `out/` after the retention rounded up to whole days plus one day, incomplete multipart uploads are aborted after 1 day (MinIO ignores this element). The janitor deletes much earlier; the rules only catch what it missed, for example during a long API outage.

**Credentials (least privilege).** The script prints IAM-style JSON accepted by AWS and MinIO. In short:

| Credential | Bucket | `in/*` | `out/*` |
|---|---|---|---|
| api | `s3:ListBucket` | `PutObject`, `GetObject` (HEAD), `DeleteObject`, `AbortMultipartUpload` | `GetObject` (HEAD and the presigned GET), `DeleteObject` |
| worker | `s3:ListBucket` | `GetObject`, `DeleteObject` | `PutObject`, `GetObject` (no-overwrite check), `DeleteObject`, `AbortMultipartUpload` |

`s3:ListBucket` is needed so that a missing object reads as "not found" (AWS answers `403` instead of `404` without it), and the janitor lists both prefixes. The api signs upload forms and download URLs with its own key, so the browser acts with the api's rights, limited by the policy of each form or URL. Neither credential can write outside `in/` and `out/`, the api cannot write outputs and the worker cannot write inputs (verified on MinIO by `tests/container/s3.sh`). The api starts even if its credential may not call `HeadBucket` (it logs a warning).

**Configuration** of api and worker: the S3 rows (`CONVERTFLOW_STORAGE`, `CONVERTFLOW_S3_ENDPOINT` and the following) in [operations.md](operations.md#variables). `CONVERTFLOW_S3_PUBLIC_ENDPOINT` is the address browsers use; the API derives the origin it adds to its Content-Security-Policy (`connect-src` and `form-action` only) from a real signature, so path-style and virtual-host-style addressing both work. An origin that is not a plain `http(s)://host[:port]` is not added, and the UI then keeps uploading through the API.

## The development and test profile (compose.s3.yaml)

**For development and tests only.** It adds a MinIO server to the stack as a local stand-in for an S3 service. MinIO is AGPL-3.0; this project does not distribute it (it is not part of any image, release or the air-gap bundle) and the official images are no longer published, so the profile pins a Chainguard source rebuild (`MINIO_IMAGE` in `scripts/tool-versions.env`, kept equal to the compose file by `scripts/check-pinned-digests.sh`). Bring your own S3 service in production.

```bash
scripts/gen-secrets.sh                                           # adds the s3_* secrets to an existing ./secrets
set -a; . docker/ffmpeg.env; set +a
podman compose -f compose.yaml -f compose.s3.yaml up -d s3       # the store first: the api refuses to start without its bucket
CONTAINER_RUNTIME=podman scripts/s3-bootstrap.sh --minio-users   # bucket, lifecycle, least-privilege api and worker users
podman compose -f compose.yaml -f compose.s3.yaml up -d          # api and worker in s3 mode
```

What the profile changes (verified by `HARDENING_PROFILE=s3 tests/container/hardening.sh`):

- The store runs non-root (65532), read-only, with no capabilities, resource limits and its admin credential as file secrets. api and worker reach it on the internal `backend` network over plain HTTP (this host only).
- Browsers reach it on `127.0.0.1:${CONVERTFLOW_S3_PORT:-9000}`, published through a separate `s3edge` network (an internal network cannot publish ports). The store therefore has egress, like the api. `MINIO_API_CORS_ALLOW_ORIGIN` allows only `CONVERTFLOW_S3_CORS_ORIGINS` (default `http://127.0.0.1:${CONVERTFLOW_PORT}`). Open the UI at exactly that origin: `http://localhost:8080` is a different origin and its uploads fail the CORS check. Serving the store under the same reverse-proxy origin as the UI is the alternative for remote access; it was not set up or tested here.
- api and worker use `CONVERTFLOW_STORAGE=s3` with separate least-privilege keys and no longer mount the shared `media` volume.
- The worker's tmpfs scratch must hold one input and one output: the profile lowers the defaults to `CONVERTFLOW_MAX_UPLOAD_BYTES` 1 GiB and `CONVERTFLOW_MAX_OUTPUT_BYTES` 2 GiB, sizes `/tmp` at 3 GiB and raises the worker memory limit to 6 GiB, because tmpfs pages count against it. Raise all four together, or mount a disk-backed volume at `/tmp`, for larger files.

`tests/container/s3.sh` replays the UI's direct upload with curl against this profile (CSP, CORS preflight, form POST, `/complete`, conversion, redirected download, `EntityTooLarge`, credential scoping, log hygiene). The same flow was also run once in a real Chrome against this profile, with the UI's own code: preflight `204`, form POST `204`, `/complete` `200`, conversion done, the download followed the redirect to the store, and cancelling during the upload deleted the job.

## Failure behavior

| Situation | What happens |
|---|---|
| Store unreachable at api start | logged as a warning; the api starts. A missing bucket or rejected credentials stop the process. |
| Store fails during an upload form POST | the UI retries a busy store (`503`, `SlowDown`) with the same form while it is valid, then reports "The storage server is busy or unavailable" and deletes the job |
| Upload larger than the limit | the store answers `400 EntityTooLarge`; the UI shows "File is larger than the server's upload limit." |
| Form expired | the store answers `403 AccessDenied`; the UI reports that the upload link may have expired. Start the upload again. |
| Browser blocked by CORS or CSP | the request fails with status 0; the UI reports that it could not reach the storage server. Check the CORS origin and `CONVERTFLOW_S3_PUBLIC_ENDPOINT`. |
| `/complete` size differs from the declared size | `409`; the object is removed, the job stays `uploading` until the UI deletes it or the janitor expires it |
| Store unavailable while a worker reads the input or writes the output | the attempt is released (not failed) and the janitor requeues the job within a minute. **Each release uses an attempt**: after `CONVERTFLOW_MAX_ATTEMPTS` the job fails with the existing message "worker lost; attempts exhausted", which is misleading for a storage outage. There is no backoff beyond the janitor's interval. |
| Output object removed by the lifecycle rule or by hand | `GET .../output` answers `410` |
| Database unavailable | unchanged from shape A |

## Cost notes

Not benchmarked. No cost, throughput or cold-start figures were measured for this profile, and none are given here. Qualitatively:

- Upload and download bytes no longer pass through the API, so the API needs little CPU and bandwidth regardless of file sizes; the store and the network to it carry the traffic instead.
- Workers need scratch space for one input plus one output instead of a shared volume, which is what later allows short-lived workers.
- Store costs depend on the provider: stored bytes (bounded by retention), requests, and egress for downloads. Inputs are deleted after processing; the lifecycle rules bound what a fault can leave behind.

## Limits and open points

- A presigned upload form can be used again until it expires, also after `/complete`: the owner of a job could replace that job's input. The damage is bounded (same key, same size limit, the worker rechecks the size, ffprobe validates as usual). See [security.md](security.md).
- The upload policy allows any size up to `CONVERTFLOW_MAX_UPLOAD_BYTES`, not just the declared size; the mismatch is caught at `/complete`.
- No storage backpressure in s3 mode besides the queue cap (there is no free-space figure for a bucket).
- Worker `--once`, scale-to-zero, sessions and quotas, and Kubernetes manifests are later phases.
