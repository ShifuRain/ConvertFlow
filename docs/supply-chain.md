# CI and release supply chain

Status: the workflows in `.github/workflows/` were validated statically (actionlint 1.7.12 with shellcheck, YAML
parse, `scripts/check-pinned-digests.sh`). They have **not run on GitHub yet**; every script they call was run locally
(rootless podman 6.1.2). Expect small first-run fixes, especially in the `hardening` job (see below).

## Workflows

| Workflow | Trigger | What it does |
|---|---|---|
| `ci.yml` | push to main, PRs, manual | `test` (gofmt, `go vet`, `go test -race` with a Postgres service, UI build + `tsc --noEmit`), `security` (govulncheck, gosec, licence policy), `pinning` (pinning policy, `.trivyignore` policy, actionlint), `images` (api + worker **lgpl** build with buildx GHA cache, Trivy), `hardening` (`tests/container/hardening.sh` + `--self-test`), `image-full` (FULL/GPL worker, **manual dispatch only**) |
| `release.yml` | tags `v*` only | gates, build + push to GHCR, cosign keyless signing, Trivy, SBOMs, THIRD-PARTY-LICENSES, SHA256SUMS, air-gap bundle, **draft** release |
| `digests.yml` | weekly + manual | `scripts/check-pinned-digests.sh --online` |

All third-party actions are pinned to full commit SHAs (tag in a comment). Top-level `permissions: contents: read`;
only the release job elevates (`contents`, `packages`, `id-token`: write). No `pull_request_target`, no secret is echoed,
CI runs are cancelled when superseded. Tool versions live in `scripts/tool-versions.env` (nothing is `@latest`).

## Policies

* **Pinning** (`scripts/check-pinned-digests.sh`, fails CI): every `FROM` and external compose image is `@sha256:`; every
  workflow `uses:` is a 40-hex SHA; workflow/tool container images are digest-pinned; no `:latest`/`@latest`.
  `--online` additionally checks digests still resolve (**fail** if not) and are still the tag's current digest
  (**warning**; `--strict` makes it a failure; the release job uses `--strict`).
* **Licences** (`scripts/check-licenses.sh`): Go dependencies must be MIT, BSD-2/3-Clause, Apache-2.0 or ISC. GPL/AGPL/LGPL
  and also MPL-2.0 are rejected (no current dependency needs MPL; add it deliberately, with a justification, if that changes).
  FFmpeg is not a Go dependency; its licence handling is described in `docker/SOURCE-OFFER.txt` and the generated
  `THIRD-PARTY-LICENSES/FFMPEG.txt`. The UI build tools are not shipped.
* **Vulnerabilities**: govulncheck (source), Trivy on the built images (HIGH/CRITICAL with an available fix fail).
  Exceptions only in `.trivyignore`, each with a `# justification:` and `# expires:` (max 90 days) line directly above;
  `scripts/check-trivyignore.sh` enforces this and fails after expiry.
* **gosec**: reviewed false positives carry an inline `#nosec Gxxx -- reason` in the code.

## Release artifacts (lgpl flavor only)

Images `ghcr.io/<owner>/<repo>/convertflow-{api,worker}:<tag>` (signed by digest), `convertflow-<tag>-lgpl-airgap.tar.gz`,
CycloneDX SBOMs (`convertflow-source`, `-api`, `-worker`), `THIRD-PARTY-LICENSES.tar.gz`, `IMAGE-REFS.txt`, `SHA256SUMS`,
and `*.sigstore.json` cosign bundles for `SHA256SUMS`, the bundle and each SBOM. Verification commands are in the
release notes. The bundle is byte-reproducible given identical image archives (checked locally: two builds, identical
SHA-256); the images themselves are not claimed to be reproducible.

## Air-gap bundle

`scripts/make-airgap-bundle.sh <version> <flavor> <outdir>` (rootless podman). Contents: `images.tar.gz`
(api, worker, postgres), `compose.yaml`, `compose.airgap.yaml`, `IMAGES.txt`, `docker/`, `scripts/gen-secrets.sh`, docs,
licences, SBOMs, `AIRGAP-README.txt`, `SHA256SUMS`. Offline: `sha256sum -c SHA256SUMS`, `gunzip -c images.tar.gz | podman load`,
`scripts/gen-secrets.sh`, then `podman compose -f compose.yaml -f compose.airgap.yaml up -d --no-build`.
The postgres digest pin in `compose.yaml` cannot survive `save`/`load` (the archive keeps only the platform image), so the
bundle re-tags it as `convertflow-postgres:<tag>` and `compose.airgap.yaml` points the `db` service at that name.

## Not verified

* The Docker engine for the product (only rootless podman 6.x is verified). CI uses Docker only to *build* images
  (buildx) and to run the Trivy container; the hardening test runs under podman, installed from the runner's distro
  packages (4.x, not the 6.x used locally) with a checksum-pinned docker-compose provider.
* Nothing has executed on GitHub Actions. Cosign signing, GHCR push and `gh release create` are untested.

## Updating pins

Base images: `CONTAINER_RUNTIME=podman scripts/pin-digests.sh`, review the diff. Actions: Dependabot (weekly), keep the SHA +
tag comment. Tool versions/digests: edit `scripts/tool-versions.env`, run `scripts/check-pinned-digests.sh --online`.
