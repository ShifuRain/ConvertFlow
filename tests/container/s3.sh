#!/usr/bin/env bash
# End-to-end test of the S3 storage profile (compose.yaml + compose.s3.yaml) against the real containers, replaying what
# the browser UI does for a direct upload, with curl (no real browser is involved):
#   presets answer ConvertFlow-Upload-Mode: presigned and the CSP allows exactly the store origin;
#   JSON create -> CORS preflight (OPTIONS with Origin) -> multipart form POST of the signed fields, then the file,
#   with the UI's Origin -> /complete -> the worker converts -> /output redirects to a presigned GET -> FLAC verified;
# plus: a foreign origin gets no CORS grant, an oversize upload is refused by the store (EntityTooLarge) with CORS
# headers so the UI can read the code, the multipart path through the API still works, the least-privilege api and
# worker credentials cannot write each other's prefix, the input is gone after processing, and no presigned
# signature or secret appears in the api or worker logs.
# Requires: podman or docker (CONTAINER_RUNTIME, default docker) with a compose provider, jq, curl, built images
# (`set -a; . docker/ffmpeg.env; set +a; $RT compose build`). Uses its own project (convertflow-s3test) and ports and
# removes it (down -v) on exit; a real deployment is never touched.
set -euo pipefail
SELF=$(readlink -f "${BASH_SOURCE[0]}")
cd "$(dirname "$SELF")/../.."
. scripts/tool-versions.env
RT=${CONTAINER_RUNTIME:-docker}
PROJECT=convertflow-s3test
export CONVERTFLOW_PORT=${CONVERTFLOW_S3TEST_PORT:-18082}
export CONVERTFLOW_S3_PORT=${CONVERTFLOW_S3TEST_S3_PORT:-18092}
export CONVERTFLOW_MAX_UPLOAD_BYTES=1048576   # small, so that an oversize upload is cheap to send
BASE=http://127.0.0.1:$CONVERTFLOW_PORT
STORE=http://127.0.0.1:$CONVERTFLOW_S3_PORT
ORIGIN=$BASE
CF=(-f compose.yaml -f compose.s3.yaml)
dc() { $RT compose -p "$PROJECT" "${CF[@]}" "$@"; }
safe_down() {
  case "$PROJECT" in convertflow-s3test*) ;; *) echo "refusing down -v for project $PROJECT" >&2; return 1 ;; esac
  dc down -v >/dev/null 2>&1 || true
}
fail=0
check() { if ! eval "$2"; then echo "FAIL: $1"; fail=1; else echo "ok:   $1"; fi; }
must() { if ! eval "$2"; then echo "FAIL: $1 (cannot continue)"; exit 1; fi; echo "ok:   $1"; }

set -a; . docker/ffmpeg.env; set +a
export FFMPEG_FLAVOR=${FFMPEG_FLAVOR:-lgpl}
scripts/gen-secrets.sh >/dev/null
tmp=$(mktemp -d)
cleanup() {
  rc=$?
  if [ "$rc" != 0 ] || [ "$fail" != 0 ]; then echo "--- container logs ---"; dc logs --tail 30 2>&1 || true; fi
  safe_down
  rm -rf "$tmp"
}
trap cleanup EXIT

# mc as a given store user; the credentials go through stdin (see scripts/s3-bootstrap.sh)
mc_as() { # user-file secret-file mc-arguments...
  local ak sk; ak=$(tr -d '\r\n' <"$1"); sk=$(tr -d '\r\n' <"$2"); shift 2
  { echo 'set -uo pipefail; export HOME=/tmp MC_CONFIG_DIR=/tmp/mc; mkdir -p /tmp/mc'
    printf 'printf %%s %q | mc alias import cf >/dev/null\n' "{\"url\":\"$STORE\",\"accessKey\":\"$ak\",\"secretKey\":\"$sk\",\"api\":\"s3v4\",\"path\":\"on\"}"
    printf 'printf x | mc'; printf ' %q' "$@"; echo; } |
    $RT run -i --rm --network host --read-only --tmpfs /tmp:size=16m --cap-drop ALL --security-opt no-new-privileges \
      --entrypoint /bin/bash "$MC_IMAGE" -s
}

safe_down
dc up -d --no-build --wait --wait-timeout 120 s3 >/dev/null 2>&1 || true
must "s3 bootstrap with least-privilege users" \
  "CONTAINER_RUNTIME=$RT S3_ADMIN_URL=$STORE scripts/s3-bootstrap.sh --minio-users >$tmp/bootstrap.log 2>&1"
dc up -d --no-build --wait --wait-timeout 180 >/dev/null 2>&1 || true
must "api is healthy" "curl -fs $BASE/healthz >/dev/null"
for _ in $(seq 1 30); do curl -fs "$BASE/api/v1/presets" | grep -q audio-flac && break; sleep 2; done
must "worker advertises audio-flac" "curl -fs $BASE/api/v1/presets | grep -q audio-flac"

# ---- what the UI sees --------------------------------------------------------------------------
curl -fs -D "$tmp/presets.h" -o /dev/null "$BASE/api/v1/presets"
check "presets advertise the presigned upload mode" "grep -qix 'ConvertFlow-Upload-Mode: presigned' <(tr -d '\r' <$tmp/presets.h)"
csp=$(curl -fs -D - -o /dev/null "$BASE/" | tr -d '\r' | sed -n 's/^[Cc]ontent-[Ss]ecurity-[Pp]olicy: //p')
check "CSP allows the store origin in connect-src and form-action, nowhere else" \
  "[[ \"\$csp\" == *\"connect-src 'self' $STORE;\"* && \"\$csp\" == *\"form-action 'self' $STORE;\"* && \$(grep -o '$STORE' <<<\"\$csp\" | wc -l) -eq 2 ]]"

# ---- direct upload, as the UI does it ------------------------------------------------------------
tool() { $RT run --rm --network none --userns=keep-id --user "$(id -u):$(id -g)" -v "$tmp:$tmp" --entrypoint "$1" \
  "convertflow-worker:${CONVERTFLOW_VERSION:-dev}-$FFMPEG_FLAVOR" "${@:2}"; }
[ "$RT" = podman ] || tool() { $RT run --rm --network none --user "$(id -u):$(id -g)" -v "$tmp:$tmp" --entrypoint "$1" \
  "convertflow-worker:${CONVERTFLOW_VERSION:-dev}-$FFMPEG_FLAVOR" "${@:2}"; }
tool ffmpeg -hide_banner -loglevel error -f lavfi -i sine=duration=2 -c:a pcm_s16le "$tmp/in.wav"
size=$(stat -c %s "$tmp/in.wav")
api() { curl -s -H "Origin: $ORIGIN" -H "Sec-Fetch-Site: same-origin" "$@"; }   # browser-like same-origin API call
api -o "$tmp/create.json" -w '%{http_code}' -H 'Content-Type: application/json' \
  -d "{\"preset\":\"audio-flac\",\"filename\":\"My Clip.wav\",\"size\":$size}" "$BASE/api/v1/jobs" >"$tmp/create.code"
must "JSON create answers 201" "[ \"\$(cat $tmp/create.code)\" = 201 ]"
id=$(jq -er .id "$tmp/create.json"); url=$(jq -er .upload.url "$tmp/create.json")
check "upload URL is on the store origin" "[[ \"$url\" == $STORE/* ]]"
pre=$(curl -s -i -X OPTIONS "$url" -H "Origin: $ORIGIN" -H 'Access-Control-Request-Method: POST' | tr -d '\r')
check "CORS preflight from the UI origin is granted" \
  "grep -qE '^HTTP/[0-9.]+ 20[04]' <<<\"\$pre\" && grep -qix 'Access-Control-Allow-Origin: $ORIGIN' <<<\"\$pre\""
evil=$(curl -s -i -X OPTIONS "$url" -H 'Origin: http://evil.example' -H 'Access-Control-Request-Method: POST' | tr -d '\r')
check "CORS preflight from a foreign origin gets no grant" "! grep -qi '^Access-Control-Allow-Origin' <<<\"\$evil\""
form=()
while IFS= read -r k; do form+=(--form-string "$k=$(jq -r --arg k "$k" '.upload.fields[$k]' "$tmp/create.json")"); done \
  < <(jq -r '.upload.fields | keys[]' "$tmp/create.json")
code=$(curl -s -o "$tmp/up.body" -D "$tmp/up.h" -w '%{http_code}' -H "Origin: $ORIGIN" "${form[@]}" -F "file=@$tmp/in.wav" "$url")
check "form POST (fields, then the file) to the store is accepted (204) with a CORS grant" \
  "[ $code = 204 ] && grep -qix 'Access-Control-Allow-Origin: $ORIGIN' <(tr -d '\r' <$tmp/up.h)"
code=$(api -o "$tmp/complete.json" -w '%{http_code}' -X POST "$BASE/api/v1/jobs/$id/complete")
check "/complete queues the job (200)" "[ $code = 200 ] && [ \"\$(jq -r .status $tmp/complete.json)\" != uploading ]"
st=""
for _ in $(seq 1 120); do
  st=$(curl -fs "$BASE/api/v1/jobs/$id" | jq -er .status); [ "$st" = done ] || [ "$st" = failed ] && break; sleep 1
done
must "job finished (status $st)" "[ $st = done ]"
curl -s -o /dev/null -D "$tmp/out.h" "$BASE/api/v1/jobs/$id/output"
loc=$(tr -d '\r' <"$tmp/out.h" | sed -n 's/^[Ll]ocation: //p')
check "/output redirects (302, no-store) to a presigned GET on the store" \
  "grep -qE '^HTTP/[0-9.]+ 302' $tmp/out.h && grep -qix 'Cache-Control: no-store' <(tr -d '\r' <$tmp/out.h) && [[ \"\$loc\" == $STORE/convertflow/out/$id.* ]]"
curl -fs -o "$tmp/out.flac" -D "$tmp/dl.h" "$loc"
check "download is the FLAC, with the job's file name" \
  "grep -qi 'filename=\"My Clip.flac\"' $tmp/dl.h && tool ffprobe -v error -show_entries format=format_name -of csv=p=0 $tmp/out.flac | grep -qx flac"
check "the input object is gone after processing, the output is there" \
  "! mc_as secrets/s3_root_user secrets/s3_root_password stat cf/convertflow/in/$id >/dev/null 2>&1 && mc_as secrets/s3_root_user secrets/s3_root_password stat cf/convertflow/out/$id.1.flac >/dev/null 2>&1"

# ---- refusals --------------------------------------------------------------------------------------
api -o "$tmp/c2.json" -H 'Content-Type: application/json' -d '{"preset":"audio-flac","filename":"big.wav","size":1048576}' "$BASE/api/v1/jobs" >/dev/null
url2=$(jq -er .upload.url "$tmp/c2.json"); id2=$(jq -er .id "$tmp/c2.json")
form=()
while IFS= read -r k; do form+=(--form-string "$k=$(jq -r --arg k "$k" '.upload.fields[$k]' "$tmp/c2.json")"); done \
  < <(jq -r '.upload.fields | keys[]' "$tmp/c2.json")
head -c 1048577 /dev/zero >"$tmp/big.bin"
code=$(curl -s -o "$tmp/big.body" -D "$tmp/big.h" -w '%{http_code}' -H "Origin: $ORIGIN" "${form[@]}" -F "file=@$tmp/big.bin" "$url2")
check "an upload over the limit is refused by the store (400 EntityTooLarge) and readable by the UI origin" \
  "[ $code = 400 ] && grep -q '<Code>EntityTooLarge</Code>' $tmp/big.body && grep -qix 'Access-Control-Allow-Origin: $ORIGIN' <(tr -d '\r' <$tmp/big.h)"
check "/complete without an upload is refused (409) and the job can be deleted" \
  "[ \"\$(api -o /dev/null -w '%{http_code}' -X POST $BASE/api/v1/jobs/$id2/complete)\" = 409 ] && [ \"\$(api -o /dev/null -w '%{http_code}' -X DELETE $BASE/api/v1/jobs/$id2)\" = 204 ]"
check "multipart upload through the API still works in s3 mode (202)" \
  "[ \"\$(curl -s -o /dev/null -w '%{http_code}' -F preset=audio-flac -F file=@$tmp/in.wav $BASE/api/v1/jobs)\" = 202 ]"

# ---- least privilege ---------------------------------------------------------------------------------
# refused for the right reason (a permission error), not because of any other failure
denied() { local out; if out=$(mc_as "$@" 2>&1); then return 1; fi; grep -qiE 'access denied|insufficient permissions' <<<"$out"; }
check "the api credential cannot write outputs" \
  "denied secrets/s3_api_access_key secrets/s3_api_secret_key pipe cf/convertflow/out/forged.1.flac"
check "the worker credential cannot write inputs" \
  "denied secrets/s3_worker_access_key secrets/s3_worker_secret_key pipe cf/convertflow/in/forged"
check "neither credential can write outside in/ and out/" \
  "denied secrets/s3_api_access_key secrets/s3_api_secret_key pipe cf/convertflow/x && denied secrets/s3_worker_access_key secrets/s3_worker_secret_key pipe cf/convertflow/x"
check "the worker credential can write outputs (positive control)" \
  "mc_as secrets/s3_worker_access_key secrets/s3_worker_secret_key pipe cf/convertflow/out/probe.1.flac >/dev/null 2>&1"

# ---- no credentials in logs ------------------------------------------------------------------------
dc logs api worker >"$tmp/app.log" 2>&1 || true
check "api and worker logs carry no signature, policy or store secret" \
  "! grep -qiE 'x-amz-signature|x-amz-credential|\"policy\"' $tmp/app.log && ! grep -qF \"\$(cat secrets/s3_api_secret_key)\" $tmp/app.log && ! grep -qF \"\$(cat secrets/s3_worker_secret_key)\" $tmp/app.log"

[ "$fail" = 0 ] && echo "s3 profile test passed"
exit "$fail"
