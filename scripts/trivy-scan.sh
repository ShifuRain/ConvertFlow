#!/usr/bin/env bash
# Scans images with the pinned Trivy container (scripts/tool-versions.env). Fails (exit 1) on any HIGH or CRITICAL
# finding that has a fix available and is not listed in .trivyignore (whose entries must carry a justification and
# an expiry: scripts/check-trivyignore.sh runs first). Needs network once to fetch the vulnerability DB.
# Usage: scripts/trivy-scan.sh <image-ref|path/to/image.tar>...   (CONTAINER_RUNTIME=podman|docker, default docker)
set -euo pipefail
cd "$(dirname "$0")/.."
. scripts/tool-versions.env
RT=${CONTAINER_RUNTIME:-docker}
[ $# -ge 1 ] || { echo "usage: $0 <image|tar>..." >&2; exit 2; }
scripts/check-trivyignore.sh
cache=${TRIVY_CACHE_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/convertflow-trivy}
mkdir -p "$cache"
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
rc=0
for img in "$@"; do
  if [ -f "$img" ]; then tar=$(readlink -f "$img"); else tar=$work/$(echo "$img" | tr '/:' '__').tar; "$RT" save -o "$tar" "$img"; fi
  chmod a+r "$tar"
  echo "=== trivy: $img"
  "$RT" run --rm --network host \
    -v "$tar:/scan/image.tar:ro" -v "$PWD/.trivyignore:/scan/.trivyignore:ro" -v "$cache:/root/.cache" \
    "$TRIVY_IMAGE" image --input /scan/image.tar --ignorefile /scan/.trivyignore \
    --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 --no-progress || rc=1
done
exit $rc
