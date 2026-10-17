# ConvertFlow

Self-hosted, air-gap-friendly web app and REST API that transcodes video and audio with FFmpeg. Dark UI with drag-and-drop, live progress, no CDN and no telemetry.

**There is no authentication.** Anyone who can reach the port can submit, list, download and delete jobs. The default compose file publishes the API on `127.0.0.1` only. Read [docs/security.md](docs/security.md) and [docs/risk-acceptance-template.md](docs/risk-acceptance-template.md) before exposing it to a network.

ConvertFlow is software, not an ISMS. It cannot be "IT-Grundschutz compliant" or certified; see [docs/grundschutz-mapping.md](docs/grundschutz-mapping.md) for how it supports an operator's own Grundschutz work. **Note:** that mapping covers the classic IT-Grundschutz-Kompendium, Edition 2023, only. The BSI is introducing Grundschutz++ (which it states will replace the Kompendium, with a transition period); this mapping does not cover it. Verify the current status at bsi.bund.de.

## Verification status

Everything below was run on **rootless podman 6.1.2 with the docker-compose provider on Linux**. The Docker engine, Windows and macOS are **not verified**. The GitHub workflows have not run on GitHub yet ([docs/supply-chain.md](docs/supply-chain.md)).

## Architecture in one paragraph

Three containers: `api` (Go, distroless, serves UI + REST + SSE, no FFmpeg), `worker` (Go + FFmpeg/ffprobe, the only component that parses untrusted media, no network egress) and `db` (PostgreSQL, the job queue and all state). `api` and `worker` share a `media` volume for input and output files. Scale throughput with more workers.

## Quick start (air-gapped)

On a connected machine, obtain the release bundle `convertflow-<version>-lgpl-airgap.tar.gz` and its `SHA256SUMS`/`*.sigstore.json` files from the release page, verify them there ([docs/supply-chain.md](docs/supply-chain.md)), and carry the bundle to the target host. Nothing below needs a network.

```bash
tar -xzf convertflow-<version>-lgpl-airgap.tar.gz && cd convertflow-<version>-lgpl
sha256sum -c SHA256SUMS                       # integrity of the transferred files
gunzip -c images.tar.gz | podman load         # docker load is UNVERIFIED
scripts/gen-secrets.sh                        # random DB passwords + connection URL into ./secrets (mode 700)
set -a; . docker/ffmpeg.env; set +a
CONVERTFLOW_VERSION=<version> FFMPEG_FLAVOR=lgpl \
  podman compose -f compose.yaml -f compose.airgap.yaml up -d --no-build
```

The bundle's own `AIRGAP-README.txt` repeats these steps with the exact version filled in. `compose.airgap.yaml` points the `db` service at the locally re-tagged PostgreSQL image, because a loaded image archive cannot keep the registry digest that `compose.yaml` pins.

> **Using a DNS name or a reverse proxy?** The API refuses every request whose `Host` header it does not recognise (HTTP `421`) to defeat DNS rebinding. Out of the box only `localhost` and IP addresses are accepted. If you open the service as `https://convert.example.org` or put a proxy in front, list the name: `CONVERTFLOW_ALLOWED_HOSTS=convert.example.org` in `.env` (comma separated; `*` disables the check). See [docs/operations.md](docs/operations.md#dns-rebinding-host-allow-list).

Open <http://127.0.0.1:8080>. The UI is the start page, the API guide is at `/docs.html` and the OpenAPI 3.1 document at `/api/openapi.json`. Check the stack:

```bash
curl http://127.0.0.1:8080/healthz            # ok
curl http://127.0.0.1:8080/api/v1/presets     # presets with at least one live worker
```

Presets appear a few seconds after the workers have registered. If the list stays empty, see "No presets" in [docs/operations.md](docs/operations.md#troubleshooting).

### First conversion in the UI

Drop one or more files on the drop zone, choose a preset (for example "Audio - FLAC"), and watch the progress bar. A finished row offers a download button and shows the encoder that was actually used. Results and metadata are deleted after the retention period (default 24 h), or immediately with the Remove/Cancel button.

### First conversion with curl

These commands were run against the podman stack described above (see [docs/api.md](docs/api.md) for the full guide).

```bash
BASE=http://127.0.0.1:8080
curl -s -F preset=audio-flac -F file=@song.wav $BASE/api/v1/jobs     # 202 + job JSON with "id"
curl -s $BASE/api/v1/jobs/<id>                                       # status, progress, encoder_used
curl -OJ $BASE/api/v1/jobs/<id>/output                               # download; Content-Disposition names the file
curl -s -X DELETE $BASE/api/v1/jobs/<id>                             # delete (204) or cancel a running job (202)
```

Send the `preset` (or JSON `options`) field **before** the `file` field; otherwise the request is rejected with 400 before any bytes are stored. SSE progress, options, error format and limits are in [docs/api.md](docs/api.md).

## FFmpeg flavors

| Flavor | Contents | Use |
|---|---|---|
| `lgpl` (**default**) | FFmpeg built without `--enable-gpl`/`--enable-nonfree`. Encoders: SVT-AV1, VP9 (libvpx), Opus, Vorbis, FLAC, MP3 (LAME), PCM/WAV. Presets: AV1, VP9 and the audio presets except AAC. | Default. No H.264/H.265/AAC encoders. |
| `full` (opt-in) | Adds x264, x265 (GPL) and enables the native AAC encoder. Adds the H.264, H.265 and AAC (M4A) presets. | Only after reading [docs/licensing.md](docs/licensing.md). The FFmpeg binary becomes GPL-licensed, and H.264/H.265/AAC are covered by patent pools (for example Via LA, Access Advance); licensing them is the operator's responsibility and may apply to internal use. This is not legal advice. |

The release bundles ship the `lgpl` flavor only. To use `full`, build it yourself (below); the build prints a licensing and patent notice.

**Hardware acceleration is not part of the default image.** The FFmpeg build uses `--disable-autodetect` and contains no NVENC/QSV/VAAPI support. See [docs/operations.md](docs/operations.md#hardware-acceleration).

## Build from source

Requires podman (verified) or docker (unverified) with compose, and network access for the base images and the FFmpeg source tarball (checksum-verified at build time).

```bash
scripts/gen-secrets.sh
set -a; . docker/ffmpeg.env; set +a
FFMPEG_FLAVOR=lgpl podman compose build         # or FFMPEG_FLAVOR=full (GPL, patent notice, see docs/licensing.md)
FFMPEG_FLAVOR=lgpl podman compose up -d         # use the same FFMPEG_FLAVOR for up
```

Run the tests with `scripts/test-db.sh` (starts a throwaway PostgreSQL container, runs `go vet` and `go test ./...`; the FFmpeg-based tests need `ffmpeg` on the PATH). Container checks: `tests/container/hardening.sh` and `scripts/smoke.sh`; details in [docs/containers.md](docs/containers.md).

## Documentation

- [docs/operations.md](docs/operations.md) - configuration reference, TLS and reverse proxy, scaling, upgrades, backup, monitoring, logging and audit events, troubleshooting
- [docs/api.md](docs/api.md) - automation guide for the REST API
- [docs/security.md](docs/security.md) - threat model, what is and is not protected; vulnerability reporting in [SECURITY.md](SECURITY.md)
- [docs/grundschutz-mapping.md](docs/grundschutz-mapping.md) - IT-Grundschutz Bausteine mapping (Kompendium Edition 2023)
- [docs/risk-acceptance-template.md](docs/risk-acceptance-template.md) - risk acceptance template for the missing authentication (German and English)
- [docs/licensing.md](docs/licensing.md) - licenses, FFmpeg flavors, patent notice, source offer
- [docs/containers.md](docs/containers.md) and [docs/supply-chain.md](docs/supply-chain.md) - container build, hardening checks, CI and release pipeline
- [CONTRIBUTING.md](CONTRIBUTING.md)

License: Apache-2.0 ([LICENSE](LICENSE)). See [docs/licensing.md](docs/licensing.md) for FFmpeg and third-party components.
