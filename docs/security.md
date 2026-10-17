# Security overview

This is a summary of the threat model and of what ConvertFlow does and does not protect. The Grundschutz view is in [grundschutz-mapping.md](grundschutz-mapping.md); the deployment view is in [operations.md](operations.md). To report a vulnerability, see [SECURITY.md](../SECURITY.md).

## Deployment assumption

ConvertFlow is designed for an isolated (air-gapped) network segment with a trusted, or at least organizationally controlled, set of users. It has **no authentication and no authorization**, by explicit operator decision. Everything a reachable client can do is available to every reachable client.

## Assets

| Asset | Where | Notes |
|---|---|---|
| Uploaded media (inputs) | `media` volume, `/data/in/<uuid>` | removed after the job finishes, on delete, and by the orphan sweep |
| Converted media (outputs) | `media` volume, `/data/out/<uuid>.<attempt>.<ext>` | kept for `CONVERTFLOW_RETENTION` (24 h default) |
| Job metadata | PostgreSQL | filename (sanitized), options, probe summary, error text, timestamps |
| Availability of CPU, memory and disk | host | the main thing an anonymous client can consume |
| Database credentials | `./secrets` on the host, mounted as `/run/secrets/*` | never placed in environment variables |

## Actors and threats

| Threat | Mitigation in the product | Remaining exposure |
|---|---|---|
| **Hostile media file** exploiting a decoder (FFmpeg/libraries) | Parsing happens only in the `worker` container: non-root (uid 65532), read-only root filesystem, all capabilities dropped, `no-new-privileges`, default seccomp, pids/memory/CPU limits, no network egress (internal `backend` network), no host mounts besides the `media` volume and secrets. The API image contains no FFmpeg and no media decoder. Inputs are probed with ffprobe first and limited by duration, pixel count and stream count; output size is capped. | A memory-safety bug in FFmpeg or a codec library could still execute code inside the worker container with its privileges: read/write access to the `media` volume (other users' files in retention) and to the database credentials. Container escape protection depends on the runtime and kernel. FFmpeg 7.1.1 is pinned; update it on security advisories. |
| **FFmpeg used as a file reader / SSRF** (playlists, concat, network protocols, `file:` references in container formats) | FFmpeg is built with `--disable-network`; the command line sets `-protocol_whitelist file` and `-format_whitelist` with the supported container formats only (playlist demuxers such as HLS/concat are not on the list). Tests: `internal/ffmpeg/whitelist_test.go`, `internal/e2e/hostile_test.go` (`TestPlaylistAndConcatInputsRejected`, `TestPlaylistInputsLeaveNoTraces`). | Defense in depth, not a proof. |
| **Command / argument injection** | No shell is involved. `ffmpeg.BuildArgs` is the only place command lines are built; user input is limited to a preset id and four validated numeric options (fixed lists/ranges). Paths are server-generated UUIDs. Tests: `internal/ffmpeg/args_test.go`, `internal/jobs/options_test.go`. | |
| **Path traversal / hostile filenames** | The client filename is never used as a path; it is sanitized (no path components, control or bidi characters, quotes) and used for display and the `Content-Disposition` header only. The delete paths refuse to remove anything outside the data directory. Tests: `TestUploadHostileFilenameStaysInsideDataDir`, `TestSanitizeFilenameHostile`, `TestContentDispositionSafe`, `TestHostileFilenamesEndToEnd`. | |
| **Decompression bombs / oversized input** | Upload size limit (8 GiB default), duration/pixel/stream limits checked after probing, output cap with `-fs`, per-run timeout (4 h default), free-space check before accepting an upload. Test: `TestDecompressionBombsRefused`. | Limits are generous by default. Tune them to the hardware ([operations.md](operations.md#configuration-reference)). |
| **Denial of service by an anonymous client** | Caps on concurrent uploads (4), active jobs (100), SSE streams (64); rolling idle limit for uploads (60 s); header read timeout (10 s); `503`/`507` with `Retry-After`; resource limits per container. | A client that can reach the service can still fill the queue and the disk up to the configured caps and occupy the CPU for the duration of the queue. No per-client rate limiting exists (there is no client identity). Use network-level controls. |
| **Reading or deleting other users' jobs** | None. Job ids are random UUIDs, but `GET /api/v1/jobs` lists the latest 200 jobs with their ids, filenames and status, and `DELETE` needs no credential. | Accepted risk; see [risk-acceptance-template.md](risk-acceptance-template.md). |
| **Web attacks against the UI** (XSS, clickjacking, MIME sniffing) | CSP `default-src 'none'; script-src 'self'; style-src 'self'; ...; frame-ancestors 'none'` (no inline script or style), `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, `X-Frame-Options: DENY`, `Cross-Origin-Resource-Policy: same-origin`, `Cache-Control: no-store` on `/api/`, HSTS when TLS is terminated by the application. No cookies or sessions. All UI assets are embedded; no external requests. Tests: `TestSecurityHeaders`, `TestSecurityHeadersOnProblemAndAPI`, `internal/ui/ui_test.go`. | There is no CSRF token or `Origin` check, so a web page open in a user's browser can submit a cross-origin multipart form POST (an upload) to a reachable instance; it cannot read the response, and cross-origin `DELETE` is blocked by the missing CORS allow-list (preflight fails). This is part of the no-authentication risk. |
| **SQL injection** | All queries use bound parameters (pgx); job ids are validated as canonical UUIDs before any lookup. | |
| **Information leakage in errors** | Problem responses carry fixed messages; internal errors are logged server-side only; FFmpeg error text is sanitized of filesystem paths. | |
| **Eavesdropping on the network path** | Optional TLS 1.2+ in the application, or a reverse proxy. | Without TLS, uploads, downloads and the whole API are plaintext. The default setup is loopback-only. |
| **Database traffic** | Internal `backend` network, credentials via file secrets. | The connection URL uses `sslmode=disable`; protection relies on the internal network only. |
| **Compromised dependencies / build** | Pinned image digests, checksum-verified FFmpeg source, permissive-license policy, govulncheck, gosec, Trivy, SBOMs, signed images ([supply-chain.md](supply-chain.md)). | The pipeline has not run on GitHub yet. |
| **Data remnants after deletion** | Retention janitor, explicit delete, orphan sweep. | Plain unlink only; use an encrypted volume ([operations.md](operations.md#deletion-storage-and-encryption)). |

## What is not protected

- Confidentiality, integrity and availability against any party that can reach the port (no authentication).
- The attribution of actions to people: audit records contain the TCP peer address only, which is the proxy address behind a reverse proxy (`X-Forwarded-For` is deliberately not trusted).
- The host: the operator is responsible for the host OS, container runtime, firewall, disk encryption, backups of the database and log retention.
- Rights to the converted content and patent licensing for the `full` flavor ([licensing.md](licensing.md)).

## Hardening in the default stack

Verified by `tests/container/hardening.sh` (static compose checks plus checks of the running containers; the weakened-config self-test proves it fails on regressions): read-only root filesystems, `cap_drop: ALL`, `no-new-privileges`, no privileged mode or host namespaces, no devices, non-root users, `CapEff=0`, seccomp active, core dumps disabled, pids/memory/CPU limits, health checks, log rotation, secrets as files, API published on loopback only, internal `backend` network, digest-pinned images. Note that the `api` container is also attached to the non-internal `frontend` network (needed for the published port), so it is not egress-isolated; the `worker` and `db` are.

## Reporting vulnerabilities

See [SECURITY.md](../SECURITY.md). Do not file vulnerabilities as public issues.
