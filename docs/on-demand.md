# On-demand deployment and S3 storage

Status: storage on an S3-compatible bucket with direct browser uploads is implemented (`CONVERTFLOW_STORAGE=s3`), and so are ephemeral workers (`--once`, `--idle-exit`) with the queue semantics for scaling to zero (`CONVERTFLOW_WORKERS_ON_DEMAND`). Per-session job isolation and quotas (`CONVERTFLOW_SESSIONS` and the quota variables) are implemented; Kubernetes manifests (KEDA) are a later phase and are **not** implemented yet. Everything marked "verified" below was run with rootless podman 6.1.2, the docker-compose provider and MinIO `RELEASE.2026-10-23T20-06-51Z` (Chainguard rebuild). No AWS S3, Cloudflare R2 or Ceph endpoint was tested.

## Contents

- [Deployment shapes](#deployment-shapes)
- [How a direct upload works](#how-a-direct-upload-works)
- [Setting up a bucket](#setting-up-a-bucket)
- [The development and test profile (compose.s3.yaml)](#the-development-and-test-profile-composes3yaml)
- [Ephemeral workers (once and idle exit)](#ephemeral-workers-once-and-idle-exit)
- [Sessions and quotas for shared deployments](#sessions-and-quotas-for-shared-deployments)
- [Failure behavior](#failure-behavior)
- [Cost notes](#cost-notes)
- [Limits and open points](#limits-and-open-points)

## Deployment shapes

| | A. Single host (default) | B. On-demand (S3) |
|---|---|---|
| Processes | api + workers + PostgreSQL from `compose.yaml` | api (always on, small) + workers started per job (`--once`) + PostgreSQL |
| Storage | `CONVERTFLOW_STORAGE=local`: the shared `media` volume (`/data/in`, `/data/out`) | `CONVERTFLOW_STORAGE=s3`: one bucket, `in/<job id>` and `out/<job id>.<attempt>.<ext>` |
| Upload path | browser -> api -> volume | browser -> bucket (presigned POST); the API never sees the bytes |
| Download path | api streams the file | api answers `302` to a presigned GET; the bytes come from the bucket |
| Worker disk | the shared volume | a private scratch directory (tmpfs in compose) holding one input and one output at a time |
| Status | released (v0.1.0) | implemented; the autoscaler setup (KEDA) is not shipped yet |

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

## Ephemeral workers (once and idle exit)

The worker binary has two modes for platforms that start a container per job (Kubernetes Job or KEDA ScaledJob, Cloud Run Jobs) or that scale a deployment to zero. Without either flag it behaves as before: it processes jobs until it is stopped.

| Flag | Environment | Behavior |
|---|---|---|
| `--once` | `CONVERTFLOW_WORKER_ONCE=true` | Claim **at most one** job, run it to a terminal state, exit. If no job is claimable it polls every `CONVERTFLOW_WORKER_POLL` until the idle exit has passed (default `30s` in once mode), then exits. The wait covers an autoscaler that starts the worker before the job is visible. |
| `--idle-exit=D` | `CONVERTFLOW_WORKER_IDLE_EXIT=D` | Keep processing jobs; exit once nothing was claimable for `D` (counted from the start or the end of the last job). Combined with `--once`, it is the once-mode wait. |

Flags take precedence over the environment. An invalid value (`--idle-exit=0s`, `CONVERTFLOW_WORKER_IDLE_EXIT=30` without unit) is a start error (exit 1). Every run, also a once run, first detects its encoders and registers them (a few seconds of trial encodes) and then claims only presets it can encode.

### Exit codes

| Code | Meaning | Suggested platform handling |
|---|---|---|
| `0` | The claimed job reached a terminal state: `done`, `failed` (bad input, limits, stall, ...) or `canceled`; those are job results, not worker failures. Also: idle exit without a job, or a signal while no job was running. | success |
| `1` | Infrastructure or configuration: invalid configuration, database unreachable at start (or every claim during the idle period failed), encoder detection failed, object store or bucket unusable at start. | failure; retry with backoff |
| `75` (`EX_TEMPFAIL`) | The job did not reach a terminal state and was given back for retry: the worker was stopped by SIGTERM/SIGINT while it ran (scale-in, spot preemption, node drain), or in once mode the object store failed while reading the input or writing the output, or the job's lease was lost. The job is requeued by the janitor (counts as an attempt). | ignore / retry; not a failure of the job |

An always-on worker (no flag) still exits 0 when it is stopped by a signal, and 1 on fatal errors.

For a Kubernetes Job, a `podFailurePolicy` keeps preemptions from counting against `backoffLimit` (*unverified: no cluster was used*):

```yaml
podFailurePolicy:
  rules:
    - action: Ignore            # preempted or scaled in: the job was released for retry
      onExitCodes: {containerName: worker, operator: In, values: [75]}
    - action: Ignore            # the node shut the pod down (spot)
      onPodConditions: [{type: DisruptionTarget}]
```

### Shutdown, preemption and attempts

On SIGTERM or SIGINT (any mode) the worker kills FFmpeg, removes the partial output, and releases the job at once: the job's lease is expired (audit event `job_released`, `reason` `shutdown`), so the janitor requeues it on its next pass (within a minute) instead of waiting `CONVERTFLOW_STALE_AFTER`. No terminal state is written, the input is kept, and the worker's later writes are impossible because every worker write is fenced by worker id and status. The release and the other writes after the run are bounded by `CONVERTFLOW_WORKER_SHUTDOWN_TIMEOUT` (default `10s`); a shutdown that hangs anyway is cut 5 s later. Set the platform's grace period (`terminationGracePeriodSeconds`, `stop_grace_period`) above that sum. In the tests, SIGTERM to exit took about 0.03 s for the bare binary and 0.34 s for `podman stop` (example below).

**Attempts.** The interrupted attempt counts exactly like a worker crash: `attempts` was incremented when the job was claimed, and the janitor's requeue fails the job with "worker lost; attempts exhausted" once `CONVERTFLOW_MAX_ATTEMPTS` (default 3) is reached. A preempted job therefore never uses up its attempts faster than a crash would, but repeated preemption can fail a job. On preemptible (spot) nodes raise `CONVERTFLOW_MAX_ATTEMPTS`. The attempt is deliberately not given back: an attempt number is also the name of the attempt's output (`<id>.<attempt>.<ext>`), and reusing it could let a slow, stopped worker's cleanup reach a later attempt's output; and a job that keeps being interrupted still ends instead of looping forever.

**Stalls.** A conversion whose FFmpeg makes no progress and is idle for `CONVERTFLOW_JOB_STALL_TIMEOUT` (default 10 minutes) is killed and failed with "conversion stalled (no progress for 10 minutes)", so a hung encoder does not keep a billed pod alive until `CONVERTFLOW_JOB_TIMEOUT` (4 h). See [operations.md](operations.md#troubleshooting).

### Scratch directory

Each worker process creates a private scratch directory under `$TMPDIR` (`/tmp`, a tmpfs in compose; use an `emptyDir` in Kubernetes). It holds the downloaded input and the output before upload with `s3` storage, and is FFmpeg's working directory with both storages. The directory is removed on every exit path: after a job, on idle exit, on errors, on a signal and on the hard stop. Tests check that nothing is left (`TestOnceModeWithS3LeavesNoScratchFiles`, `TestBinarySIGTERMReleasesJobForAnotherWorker`).

### Queue semantics without live workers (`CONVERTFLOW_WORKERS_ON_DEMAND`)

With workers scaled to zero there is no live worker most of the time. Two places relied on one:

1. **Which presets the API offers.** Normally only presets a live worker (seen within 30 s) can run are listed and accepted. With `CONVERTFLOW_WORKERS_ON_DEMAND=true` the API uses the worker registrations of the last 7 days instead: every worker registers its encoders before claiming, also in once mode. A fresh installation has none yet, so run one worker once (`--once --idle-exit=1s`, it exits 0 after registering) before the first upload. The janitor deletes registrations older than 7 days.
2. **Queue expiry.** Normally `CONVERTFLOW_MAX_QUEUE_AGE` fails only jobs no live worker can run; while scaled to zero that would be every old job. With `CONVERTFLOW_WORKERS_ON_DEMAND=true` the janitor expires queued jobs purely by age ("no worker picked up this job in time"). Size `MAX_QUEUE_AGE` to the longest backlog your maximum worker count may build up.

Set it on the api (the janitor runs there); `compose.yaml` passes it through. The always-on default is unchanged; issue #9 (a short gap in a live worker's registration refresh could fail old jobs) is fixed for that mode: the janitor's live window is now at least 2 minutes or twice `STALE_AFTER`, and workers refresh their registration before a re-detection.

### Example: one worker per job with podman

Verified with rootless podman 6.1.2 against the compose stack (project name `convertflow`, `local` storage, `lgpl` worker image built from this branch). The api and database run as usual, with no always-on worker; each `podman run` is what an autoscaler would start for one queued job. The worker options mirror the `worker` service of `compose.yaml`.

```bash
set -a; . docker/ffmpeg.env; set +a
CONVERTFLOW_WORKERS_ON_DEMAND=true podman compose up -d db api      # no worker service

# 1. Register the worker image's encoders once (a fresh installation offers no presets before this).
podman run --rm --network convertflow_backend \
    --read-only --tmpfs /tmp:size=2g,mode=1777 --cap-drop ALL --security-opt no-new-privileges \
    --pids-limit 512 --memory 4g --cpus 4 \
    -v convertflow_media:/data -v ./secrets/database_url:/run/secrets/database_url:ro \
    -e CONVERTFLOW_DATABASE_URL_FILE=/run/secrets/database_url -e CONVERTFLOW_FLAVOR=lgpl \
    localhost/convertflow-worker:dev-lgpl --once --idle-exit=1s; echo "exit $?"

# 2. Queue a job, then start one worker for it (same command, without --idle-exit=1s).
curl -sS -F preset=audio-flac -F file=@tone.wav http://127.0.0.1:8080/api/v1/jobs
podman run --rm --name cf-once --network convertflow_backend \
    --read-only --tmpfs /tmp:size=2g,mode=1777 --cap-drop ALL --security-opt no-new-privileges \
    --pids-limit 512 --memory 4g --cpus 4 \
    -v convertflow_media:/data -v ./secrets/database_url:/run/secrets/database_url:ro \
    -e CONVERTFLOW_DATABASE_URL_FILE=/run/secrets/database_url -e CONVERTFLOW_FLAVOR=lgpl \
    localhost/convertflow-worker:dev-lgpl --once; echo "exit $?"
```

What happened (port 18085 instead of 8080 on the test host):

| Step | Result |
|---|---|
| `GET /api/v1/presets` before step 1 | `[]` (no registration yet) |
| step 1 | `exit 0` after 1.8 s (`jobs=0 idle_exit=true`); presets now listed |
| step 2, a 3 s WAV to `audio-flac` | `exit 0`, job `done`, attempt 1 |
| step 2 with a 60 s 720p clip to `video-vp9`, `podman stop -t 30 cf-once` at 11 % progress | `podman stop` returned after 0.34 s; `exit 75`; audit `job_released` `reason` `shutdown`; job still `running` with `heartbeat_at = -infinity`, no partial output in `out/` |
| the janitor's next pass (41 s later) | `jobs_recovered` `requeued 1`; job `queued` |
| step 2 again | `exit 0`, job `done` on attempt 2 |

Tear down with `podman compose down -v` (removes the volumes).

## Sessions and quotas for shared deployments

A deployment that many people reach (the usual reason to scale workers on demand) should not show every job to everyone. Set `CONVERTFLOW_SESSIONS=cookie` (anonymous browser sessions) or `header` (user from an authenticating reverse proxy, with `CONVERTFLOW_TRUSTED_PROXIES`), and bound what one owner can queue; details in [operations.md](operations.md#sessions-and-quotas) and [security.md](security.md).

- Worker capacity, and so cost, is bounded by the maximum worker count and `CONVERTFLOW_MAX_QUEUED_JOBS`; the per-owner quotas only share that capacity more fairly. A queue that one session cannot flood also keeps the autoscaler from starting workers for one client's backlog.
- All quota state is in PostgreSQL (counts of the jobs table plus the `rate_events` table), checked in one transaction under an advisory lock: several API replicas enforce the same limits, and a restart forgets nothing.
- The direct upload counts the announced size against the queued-bytes quota from the moment the form is issued (`/complete` verifies it), so a client cannot stage more than its byte quota in the bucket through unfinished uploads. Presigned forms and download URLs are issued only to the job's owner.
- Behind a load balancer or ingress, set `CONVERTFLOW_TRUSTED_PROXIES` to its addresses, or all clients share one per-address budget (including the 60 new sessions per hour of cookie mode).
- Verified with the `compose.s3.yaml` profile in cookie mode: a real Chrome uploaded straight to MinIO (presigned mode) and downloaded through the redirect with the session cookie on the API only. Not verified: Kubernetes, several API replicas, an authenticating proxy.

## Failure behavior

| Situation | What happens |
|---|---|
| Store unreachable at api start | logged as a warning; the api starts. A missing bucket or rejected credentials stop the process. |
| Store fails during an upload form POST | the UI retries a busy store (`503`, `SlowDown`) with the same form while it is valid, then reports "The storage server is busy or unavailable" and deletes the job |
| Upload larger than the limit | the store answers `400 EntityTooLarge`; the UI shows "File is larger than the server's upload limit." |
| Form expired | the store answers `403 AccessDenied`; the UI reports that the upload link may have expired. Start the upload again. |
| Browser blocked by CORS or CSP | the request fails with status 0; the UI reports that it could not reach the storage server. Check the CORS origin and `CONVERTFLOW_S3_PUBLIC_ENDPOINT`. |
| `/complete` size differs from the declared size | `409`; the object is removed, the job stays `uploading` until the UI deletes it or the janitor expires it |
| Worker stopped (SIGTERM, preemption) while a job runs | FFmpeg is killed, the job is released and requeued within a minute; exit code `75` for an ephemeral worker. Counts as an attempt. |
| FFmpeg hangs | the stall watchdog fails the job after `CONVERTFLOW_JOB_STALL_TIMEOUT` with "conversion stalled (no progress for N minutes)" |
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
- Kubernetes manifests with a KEDA trigger are a later phase. Scale-to-zero was exercised with `podman run` only, not on a cluster; no cold-start figures were measured.
- An autoscaler that counts every queued job (the simple KEDA query) keeps starting workers for a job no worker image can run; each exits 0 after its idle period, until `MAX_QUEUE_AGE` fails the job. Filter the trigger query by preset if you run several worker images.
- A preempted job restarts from the beginning, and every interruption uses an attempt.
