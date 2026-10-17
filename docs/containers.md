# Containers

Images: `convertflow-api` (distroless static, no shell, no ffmpeg) and `convertflow-worker`
(Debian slim + FFmpeg built from the pinned, checksum-verified source in `docker/ffmpeg.env`).

Workflow (podman or docker; set `CONTAINER_RUNTIME`):

    scripts/pin-digests.sh          # outside the air gap, before every release: resolves real digests, review `git diff`
    scripts/gen-secrets.sh          # once: writes ./secrets (git-ignored, umask 077): bootstrap and application DB passwords + URL
    set -a; . docker/ffmpeg.env; set +a
    FFMPEG_FLAVOR=lgpl podman compose build     # or FFMPEG_FLAVOR=full (GPL + x264/x265, prints a patent/licence notice)
    tests/container/hardening.sh    # static + runtime hardening checks (starts the stack if needed)
    scripts/smoke.sh                # end-to-end: upload, convert, download, ffprobe

Bumping FFmpeg: download the tarball and its `.asc` from ffmpeg.org, verify with GPG against the
release key (fingerprint FCF9 86EA 15E6 E293 A564 4F10 B432 2F04 D676 58D8, as published at
https://ffmpeg.org/download.html), compute `sha256sum`, and update `docker/ffmpeg.env`. The image
build re-checks the hash and fails closed; it never contacts a keyserver.

The API listens on `127.0.0.1:8080` only; change the host IP in `compose.yaml` deliberately.
The worker and database sit on an internal network with no egress.

## Notes and limits

- Verified: **rootless podman 6.1** locally and **podman 4.9** (ubuntu-24.04 CI runner), both with the docker-compose provider. The Docker engine is NOT verified.
- **podman 4.x does not apply the compose `ulimits`** (`core: 0` and `nofile` are absent from the container's `HostConfig.Ulimits`), so
  on podman 4 the core-dump hard limit stays at the host default (observed: soft 0, hard unlimited). `tests/container/hardening.sh`
  keeps asserting the setting in the compose model everywhere, asserts RLIMIT_CORE 0/0 at runtime on podman 5+ and docker, and on
  podman <5 only asserts that the effective soft limit is 0 (the check name says so). Use podman 5+ if the hard limit matters to you.
- **DNS forwarding on podman 4.x (known limitation).** On podman 4.9 the aardvark-dns resolver of the `internal` backend network
  forwards external lookups intermittently; this was observed repeatedly in CI. TCP egress from the worker stays blocked (that
  check is strict everywhere), but name resolution can leak. CI downgrades only the "cannot resolve external names" probe to a
  warning (`WARN:` line and a GitHub Actions `::warning::`) on podman <5; on podman 5+ and docker it is a hard failure. Compose
  still must declare the network internal. `--self-test` proves both paths with a simulated resolution.
- Pinned base-image digests go stale. Run `scripts/pin-digests.sh` before each release; `scripts/check-pinned-digests.sh --online` (weekly workflow, strict on release) detects drift. See docs/supply-chain.md.
- `scripts/smoke.sh` and `tests/container/hardening.sh` use their own compose projects (`convertflow-smoke`,
  `convertflow-hardening`) and host ports (18080 / 18081); they refuse `down -v` for any other project and never touch a
  real `convertflow` deployment. `tests/container/hardening.sh --self-test` proves a weakened config is rejected.
- Worker images are tagged per flavor: `convertflow-worker:dev-lgpl` / `:dev-full`. `FFMPEG_FLAVOR` is shared with the API
  (it filters presets by flavor) through one compose anchor together with job timeout / retention / stale settings.
- The final worker image build fails if ffmpeg/ffprobe have unresolved libraries, expected encoders are missing, or an lgpl
  build shows `--enable-gpl`/`--enable-nonfree`/`--enable-version3`.

Operating the stack (configuration, TLS, backup, monitoring): see [operations.md](operations.md). Documentation consistency is checked by `scripts/check-docs.sh` (also run in CI).

## Known limitation: DNS from the internal network on podman 4.x

The worker and database sit on an `internal` network, which blocks routed egress. On podman 4.x with
aardvark-dns, one CI run observed an external name resolving from the worker; the later runs timed out
(isolation held) and the cause was not found. Treat DNS forwarding as **not ruled out on podman 4.x**:
a compromised worker could in principle use DNS lookups as a covert channel. The hardening test now probes
this three times and fails on any answer. On podman 6.x the same probe passes locally. Air-gapped
operators should use podman 5 or newer (or docker, which is unverified) and block DNS at the host
firewall for the container networks.
