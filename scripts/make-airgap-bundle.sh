#!/usr/bin/env bash
# Builds the offline ("air-gap") install bundle:
#   convertflow-<version>-<flavor>-airgap.tar.gz
#     images.tar.gz          api + worker + pinned postgres images (`save`), gzip -n
#     compose.yaml + compose.airgap.yaml (db -> local postgres name), IMAGES.txt, docker/, scripts/gen-secrets.sh, README.md, LICENSE, docs/*.md
#     THIRD-PARTY-LICENSES/  (if $LICENSES_DIR is set)      sbom/*.cdx.json (if $SBOM_DIR is set)
#     AIRGAP-README.txt      offline load + verify + start instructions
#     SHA256SUMS             checksums of every file in the bundle
# Reproducibility: file order, owner, mtimes (SOURCE_DATE_EPOCH, default = last commit time) and gzip headers are
# normalised, so the same image archives yield a byte-identical bundle. (Image archives themselves are reproducible
# only if the images are: that depends on the builder, so the release workflow publishes the SHA-256 of what it built.)
# Only podman has been verified (docker save/load of a digest-only postgres ref is not).
# Usage: CONTAINER_RUNTIME=podman scripts/make-airgap-bundle.sh <version> <flavor> <outdir>
# Env:   API_IMAGE (default convertflow-api:<version>)  WORKER_IMAGE (default convertflow-worker:<version>-<flavor>)
#        POSTGRES_IMAGE (default: the pinned ref in compose.yaml)  LICENSES_DIR  SBOM_DIR  SOURCE_DATE_EPOCH
set -euo pipefail
cd "$(dirname "$0")/.."
RT=${CONTAINER_RUNTIME:-docker}
version=${1:?version} flavor=${2:?flavor lgpl|full} outdir=${3:?outdir}
[[ "$version" =~ ^[A-Za-z0-9._-]+$ ]] || { echo "bad version '$version'" >&2; exit 2; }
case "$flavor" in lgpl|full) ;; *) echo "flavor must be lgpl or full" >&2; exit 2 ;; esac
api=${API_IMAGE:-convertflow-api:$version}
worker=${WORKER_IMAGE:-convertflow-worker:$version-$flavor}
pg=${POSTGRES_IMAGE:-$(grep -oE 'docker\.io/library/postgres:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}' compose.yaml | head -n1)}
[ -n "$pg" ] || { echo "no pinned postgres image in compose.yaml" >&2; exit 1; }
export SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct)}

# compose.yaml expects exactly these names; tag the inputs so `up --no-build` works after loading.
want_api=convertflow-api:$version want_worker=convertflow-worker:$version-$flavor
[ "$api" = "$want_api" ] || "$RT" tag "$api" "$want_api"
[ "$worker" = "$want_worker" ] || "$RT" tag "$worker" "$want_worker"
"$RT" image exists "$pg" 2>/dev/null || "$RT" pull -q "$pg" >/dev/null

stage=$(mktemp -d); trap 'rm -rf "$stage"' EXIT
b=$stage/convertflow-$version-$flavor
mkdir -p "$b/docker" "$b/scripts" "$b/docs"
fmt=(); [ "$RT" = podman ] && fmt=(--format docker-archive --multi-image-archive)
# A saved/loaded archive carries only the platform image: it loses the registry name AND the multi-arch index digest
# that compose.yaml pins, so an offline `up` would try to pull. Therefore the postgres image is saved under a local
# name and compose.airgap.yaml points the db service at it. The pinned ref is recorded in IMAGES.txt.
pg_tag=${pg#*postgres:}; pg_tag=${pg_tag%%@*}
want_pg=convertflow-postgres:$pg_tag
"$RT" tag "$("$RT" image inspect "$pg" --format '{{.Id}}')" "$want_pg"
"$RT" save "${fmt[@]}" -o "$stage/images.tar" "$want_api" "$want_worker" "$want_pg"
gzip -9nc "$stage/images.tar" > "$b/images.tar.gz"
cp compose.yaml README.md LICENSE "$b/"
cp docker/ffmpeg.env docker/SOURCE-OFFER.txt "$b/docker/"
cp scripts/gen-secrets.sh "$b/scripts/"
for f in docs/*.md; do [ -f "$f" ] && cp "$f" "$b/docs/"; done
[ -z "${LICENSES_DIR:-}" ] || cp -r "$LICENSES_DIR" "$b/THIRD-PARTY-LICENSES"
[ -z "${SBOM_DIR:-}" ] || { mkdir "$b/sbom"; cp "$SBOM_DIR"/*.cdx.json "$b/sbom/"; }
printf 'services:\n  db:\n    image: %s   # local name of the image loaded from images.tar.gz\n' "$want_pg" > "$b/compose.airgap.yaml"
printf '%s\n%s\n%s\n' "$want_api" "$want_worker" "$want_pg  <- $pg" > "$b/IMAGES.txt"
cat > "$b/AIRGAP-README.txt" <<R
ConvertFlow $version ($flavor) - offline install bundle

1. Verify (on the transfer medium, before loading anything):
     sha256sum -c SHA256SUMS
   and, on a connected machine, verify the bundle's own signature/checksum from the release (see docs/supply-chain.md).
2. Load the images (no registry access needed). Rootless podman 6.x is the verified runtime; the Docker engine is not.
     gunzip -c images.tar.gz | podman load        # or: docker load  (UNVERIFIED for this project)
   This provides: $want_api, $want_worker and $want_pg
   ($want_pg is the postgres image pinned in compose.yaml as
     $pg
   re-tagged locally, because a loaded archive cannot keep the registry digest; see IMAGES.txt.)
3. Create secrets, then start without building:
     scripts/gen-secrets.sh
     set -a; . docker/ffmpeg.env; set +a
     CONVERTFLOW_VERSION=$version FFMPEG_FLAVOR=$flavor podman compose -f compose.yaml -f compose.airgap.yaml up -d --no-build
4. The API listens on 127.0.0.1:8080 only. See README.md and docs/containers.md.

Licences: THIRD-PARTY-LICENSES/ (FFmpeg flavor: $flavor; written source offer in docker/SOURCE-OFFER.txt).
SBOMs (CycloneDX): sbom/.
R
( cd "$b" && find . -type f ! -name SHA256SUMS -print0 | LC_ALL=C sort -z | xargs -0 sha256sum > SHA256SUMS )
mkdir -p "$outdir"; out=$(readlink -f "$outdir")/convertflow-$version-$flavor-airgap.tar.gz
tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="@$SOURCE_DATE_EPOCH" --format=gnu \
    -C "$stage" -cf - "convertflow-$version-$flavor" | gzip -9n > "$out"
( cd "$(dirname "$out")" && sha256sum "$(basename "$out")" )
echo "wrote $out"
