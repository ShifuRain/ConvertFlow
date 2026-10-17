#!/usr/bin/env bash
# Rewrites every external base image reference in Dockerfile.* and compose.yaml to <ref>@sha256:<digest>,
# where the digest is the multi-arch index digest resolved from the registry by the container runtime
# (never typed by hand). Run OUTSIDE the air gap, review `git diff`, commit. Re-run to refresh.
# Usage: CONTAINER_RUNTIME=podman scripts/pin-digests.sh
set -euo pipefail
cd "$(dirname "$0")/.."
RT=${CONTAINER_RUNTIME:-docker}
refs=(
  docker.io/library/node:22-bookworm-slim
  docker.io/library/golang:1.25-bookworm
  gcr.io/distroless/static-debian12:nonroot
  docker.io/library/debian:bookworm-slim
  docker.io/library/postgres:16-bookworm
)

index_digest() {
  local ref=$1 repo d mt
  "$RT" pull -q "$ref" >/dev/null
  repo=${ref%:*}
  # RepoDigests lists the index and the platform manifest; keep the one that is an index.
  for d in $("$RT" image inspect "$ref" --format '{{range .RepoDigests}}{{println .}}{{end}}' | grep "^${repo}@sha256:" | sed 's/.*@//'); do
    if [ "$RT" = podman ]; then
      mt=$("$RT" manifest inspect "${repo}@${d}" 2>/dev/null | grep -m1 '"mediaType"' || true)
      case "$mt" in *index*|*manifest.list*) echo "$d"; return 0 ;; esac
    else
      echo "$d"; return 0
    fi
  done
  # Single-platform image (no index): fall back to the image digest.
  "$RT" image inspect "$ref" --format '{{.Digest}}'
}

files=(Dockerfile.api Dockerfile.worker compose.yaml .github/workflows/ci.yml)
for ref in "${refs[@]}"; do
  digest=$(index_digest "$ref")
  [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "could not resolve a digest for $ref" >&2; exit 1; }
  esc=$(printf '%s' "$ref" | sed 's/[.[\*^$/]/\\&/g')
  sed -i -E "s#${esc}(@sha256:[0-9a-f]+)?([[:space:]\"']|\$)#${ref}@${digest}\2#g" "${files[@]}"
  echo "pinned $ref -> $digest"
done
