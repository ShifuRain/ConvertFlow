#!/usr/bin/env bash
# End-to-end smoke test against the real containers: bring the stack up, upload a generated WAV with
# an lgpl preset (audio-flac), wait for completion, download the result and verify it with ffprobe.
# Usage: CONTAINER_RUNTIME=podman scripts/smoke.sh      (FFMPEG_FLAVOR=lgpl|full, default lgpl)
set -euo pipefail
cd "$(dirname "$0")/.."
RT=${CONTAINER_RUNTIME:-docker}
# Dedicated project + port: never touches a real `convertflow` deployment or its volumes.
PROJECT=convertflow-smoke
export CONVERTFLOW_PORT=${CONVERTFLOW_SMOKE_PORT:-18080}
BASE=http://127.0.0.1:$CONVERTFLOW_PORT
dc() { $RT compose -p "$PROJECT" "$@"; }
safe_down() {
  case "$PROJECT" in convertflow-smoke*) ;; *) echo "refusing down -v for project $PROJECT" >&2; return 1 ;; esac
  dc down -v >/dev/null 2>&1 || true
}
set -a; . docker/ffmpeg.env; set +a
export FFMPEG_FLAVOR=${FFMPEG_FLAVOR:-lgpl}
scripts/gen-secrets.sh

tmp=$(mktemp -d)
cleanup() {
  rc=$?
  [ "$rc" = 0 ] || { echo "--- smoke test FAILED; container logs ---"; dc logs --tail 40 2>&1 || true; }
  safe_down
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
    "convertflow-worker:${CONVERTFLOW_VERSION:-dev}-$FFMPEG_FLAVOR" "$@"
}

safe_down
dc up -d --build
for _ in $(seq 1 60); do curl -fs "$BASE/healthz" >/dev/null && break; sleep 2; done
curl -fs "$BASE/healthz" >/dev/null || { echo "API did not become healthy"; exit 1; }

run_job() { # preset input output
  local preset=$1 in=$2 out=$3 id st=""
  id=$(curl -fs -F "preset=$preset" -F "file=@$in" "$BASE/api/v1/jobs" | jq -er .id)
  echo "job $id ($preset) submitted"
  for _ in $(seq 1 120); do
    st=$(curl -fs "$BASE/api/v1/jobs/$id" | jq -er .status)
    [ "$st" = done ] && break
    [ "$st" = failed ] && { echo "job $preset failed"; exit 1; }
    sleep 1
  done
  [ "$st" = done ] || { echo "job $preset did not finish (status $st)"; exit 1; }
  curl -fs -o "$out" "$BASE/api/v1/jobs/$id/output"
}

tool ffmpeg -hide_banner -loglevel error -f lavfi -i sine=duration=2 -c:a pcm_s16le "$tmp/in.wav"
for _ in $(seq 1 30); do
  curl -fs "$BASE/api/v1/presets" | grep -q audio-flac && break; sleep 2
done
run_job audio-flac "$tmp/in.wav" "$tmp/out.flac"
tool ffprobe -v error -show_entries format=format_name -of csv=p=0 "$tmp/out.flac" | grep -q flac
echo "lgpl preset audio-flac ok"

if [ "$FFMPEG_FLAVOR" = full ]; then
  tool ffmpeg -hide_banner -loglevel error -f lavfi -i testsrc=duration=2:size=320x240:rate=15 -pix_fmt yuv420p "$tmp/in.mkv"
  curl -fs "$BASE/api/v1/presets" | grep -q video-h264 || { echo "full worker does not advertise video-h264"; exit 1; }
  run_job video-h264 "$tmp/in.mkv" "$tmp/out.mp4"
  tool ffprobe -v error -select_streams v:0 -show_entries stream=codec_name -of csv=p=0 "$tmp/out.mp4" | grep -qx h264
  echo "full preset video-h264 ok"
fi
echo "smoke test passed ($FFMPEG_FLAVOR)"
