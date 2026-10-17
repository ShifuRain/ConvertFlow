#!/usr/bin/env bash
# End-to-end smoke test against the real containers: bring the stack up, upload a generated WAV with
# an lgpl preset (audio-flac), wait for completion, download the result and verify it with ffprobe.
# Usage: CONTAINER_RUNTIME=podman scripts/smoke.sh      (FFMPEG_FLAVOR=lgpl|full, default lgpl)
set -euo pipefail
cd "$(dirname "$0")/.."
RT=${CONTAINER_RUNTIME:-docker}
BASE=${CONVERTFLOW_BASE_URL:-http://127.0.0.1:8080}
set -a; . docker/ffmpeg.env; set +a
export FFMPEG_FLAVOR=${FFMPEG_FLAVOR:-lgpl}
scripts/gen-secrets.sh

tmp=$(mktemp -d)
cleanup() {
  rc=$?
  [ "$rc" = 0 ] || { echo "--- smoke test FAILED; container logs ---"; $RT compose logs --tail 40 2>&1 || true; }
  $RT compose down -v >/dev/null 2>&1 || true
  rm -rf "$tmp"
  exit "$rc"
}
trap cleanup EXIT

# ffmpeg/ffprobe: use the host's if present, otherwise the ones inside the worker image (no network).
tool() {
  local t=$1; shift
  if command -v "$t" >/dev/null 2>&1; then "$t" "$@"; return; fi
  local extra=()
  [ "$RT" = podman ] && extra+=(--userns=keep-id)
  $RT run --rm --network none "${extra[@]}" --user "$(id -u):$(id -g)" -v "$tmp:$tmp" --entrypoint "$t" \
    "convertflow-worker:${CONVERTFLOW_VERSION:-dev}" "$@"
}

$RT compose up -d --build
for _ in $(seq 1 60); do curl -fs "$BASE/healthz" >/dev/null && break; sleep 2; done
curl -fs "$BASE/healthz" >/dev/null || { echo "API did not become healthy"; exit 1; }

tool ffmpeg -hide_banner -loglevel error -f lavfi -i sine=duration=2 -c:a pcm_s16le "$tmp/in.wav"
for _ in $(seq 1 30); do
  curl -fs "$BASE/api/v1/presets" | grep -q audio-flac && break; sleep 2
done
id=$(curl -fs -F preset=audio-flac -F "file=@$tmp/in.wav" "$BASE/api/v1/jobs" | jq -er .id)
echo "job $id submitted"
st=""
for _ in $(seq 1 90); do
  st=$(curl -fs "$BASE/api/v1/jobs/$id" | jq -er .status)
  [ "$st" = done ] && break
  [ "$st" = failed ] && { echo "job failed"; exit 1; }
  sleep 1
done
[ "$st" = done ] || { echo "job did not finish (status $st)"; exit 1; }
curl -fs -o "$tmp/out.flac" "$BASE/api/v1/jobs/$id/output"
tool ffprobe -v error -show_entries format=format_name -of csv=p=0 "$tmp/out.flac" | grep -q flac
echo "smoke test passed"
