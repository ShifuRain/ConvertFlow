#!/usr/bin/env bash
# Generates CycloneDX JSON SBOMs with the pinned Syft container (scripts/tool-versions.env), fully offline:
#   <outdir>/convertflow-source.cdx.json   Go modules (go.mod/go.sum) + npm lockfile of the source tree
#   <outdir>/<name>.cdx.json               one per image archive passed as NAME=path/to/docker-or-oci-archive.tar
# Usage: CONTAINER_RUNTIME=podman scripts/gen-sbom.sh <outdir> <version> api=api.tar worker=worker.tar
set -euo pipefail
cd "$(dirname "$0")/.."
. scripts/tool-versions.env
RT=${CONTAINER_RUNTIME:-docker}
out=$1 version=$2; shift 2
mkdir -p "$out"; out=$(readlink -f "$out"); chmod a+rwx "$out"
syft() { "$RT" run --rm --network none -e SYFT_CHECK_FOR_APP_UPDATE=false "$@"; }
syft -v "$PWD:/src:ro" -v "$out:/out" "$SYFT_IMAGE" dir:/src \
  --exclude './ui/node_modules/**' --exclude './.git/**' --exclude './.superpowers/**' \
  --source-name convertflow --source-version "$version" -o cyclonedx-json=/out/convertflow-source.cdx.json -q
for spec in "$@"; do
  name=${spec%%=*} tar=$(readlink -f "${spec#*=}")
  syft -v "$tar:/in/image.tar:ro" -v "$out:/out" "$SYFT_IMAGE" docker-archive:/in/image.tar \
    --source-name "convertflow-$name" --source-version "$version" -o "cyclonedx-json=/out/convertflow-$name.cdx.json" -q
done
ls -l "$out"/*.cdx.json
