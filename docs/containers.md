# Containers

Images: `convertflow-api` (distroless static, no shell, no ffmpeg) and `convertflow-worker`
(Debian slim + FFmpeg built from the pinned, checksum-verified source in `docker/ffmpeg.env`).

Workflow (podman or docker; set `CONTAINER_RUNTIME`):

    scripts/pin-digests.sh          # outside the air gap, before every release: resolves real digests, review `git diff`
    scripts/gen-secrets.sh          # once: writes ./secrets (git-ignored, umask 077)
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

- Only **rootless podman 6.x** (with the docker-compose provider) has been verified. The Docker engine is NOT verified.
- Pinned base-image digests go stale. Run `scripts/pin-digests.sh` before each release; `scripts/check-pinned-digests.sh --online` (weekly workflow, strict on release) detects drift. See docs/supply-chain.md.
- `scripts/smoke.sh` and `tests/container/hardening.sh` use their own compose projects (`convertflow-smoke`,
  `convertflow-hardening`) and host ports (18080 / 18081); they refuse `down -v` for any other project and never touch a
  real `convertflow` deployment. `tests/container/hardening.sh --self-test` proves a weakened config is rejected.
- Worker images are tagged per flavor: `convertflow-worker:dev-lgpl` / `:dev-full`. `FFMPEG_FLAVOR` is shared with the API
  (it filters presets by flavor) through one compose anchor together with job timeout / retention / stale settings.
- The final worker image build fails if ffmpeg/ffprobe have unresolved libraries, expected encoders are missing, or an lgpl
  build shows `--enable-gpl`/`--enable-nonfree`/`--enable-version3`.
