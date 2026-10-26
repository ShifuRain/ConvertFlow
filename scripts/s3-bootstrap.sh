#!/usr/bin/env bash
# Prepares an S3-compatible bucket for CONVERTFLOW_STORAGE=s3. Safe to run again (every step is idempotent).
#   1. creates the bucket (never public; no anonymous policy is set)
#   2. sets the bucket CORS rule: allowed origin(s) = the UI origin, methods POST and GET, no extra headers.
#      MinIO does not implement bucket CORS (NotImplemented); it is configured server-wide with
#      MINIO_API_CORS_ALLOW_ORIGIN instead (compose.s3.yaml does), and the script then only warns.
#   3. sets a lifecycle backstop: in/ expires after 2 days, out/ after RETENTION + 1 day (rounded up to whole days),
#      incomplete multipart uploads are aborted after 1 day (stores that ignore that element, like MinIO, drop it).
#      The janitor normally deletes far earlier; the rules only catch what it missed.
#   4. prints the least-privilege policies for the api and worker credentials (IAM JSON, accepted by AWS and MinIO);
#      with --minio-users it also creates the two MinIO users (keys from secrets/s3_{api,worker}_{access,secret}_key)
#      and attaches the policies (MinIO admin API; used by the dev/test stack of compose.s3.yaml).
# Usage: scripts/s3-bootstrap.sh [--minio-users] | --print-policies
# Environment (defaults suit compose.s3.yaml on this host):
#   CONTAINER_RUNTIME       docker (default) or podman; runs the pinned MinIO client image (MC_IMAGE, scripts/tool-versions.env)
#   S3_ADMIN_URL            store URL reachable from this host, default http://127.0.0.1:${CONVERTFLOW_S3_PORT:-9000}
#   S3_ADMIN_USER_FILE      file with the admin access key, default secrets/s3_root_user
#   S3_ADMIN_PASSWORD_FILE  file with the admin secret key, default secrets/s3_root_password
#   CONVERTFLOW_S3_CORS_ORIGINS         comma-separated UI origins, default http://127.0.0.1:${CONVERTFLOW_PORT:-8080}
#   CONVERTFLOW_S3_BUCKET   default convertflow;  CONVERTFLOW_S3_REGION  default us-east-1
#   CONVERTFLOW_RETENTION   the api's retention (Go duration with h/m/s units), default 24h
# Credentials are passed to the client container through stdin only: never in an argument list, the container
# environment or a mounted file. The client container runs read-only, with no capabilities, on the host network
# (so that 127.0.0.1 is this host).
set -euo pipefail
cd "$(dirname "$0")/.."
. scripts/tool-versions.env
RT=${CONTAINER_RUNTIME:-docker}
mode=bootstrap users=0
for a in "$@"; do
  case "$a" in
    --minio-users) users=1 ;;
    --print-policies) mode=print ;;
    *) echo "unknown option $a" >&2; exit 2 ;;
  esac
done
bucket=${CONVERTFLOW_S3_BUCKET:-convertflow}
region=${CONVERTFLOW_S3_REGION:-us-east-1}
[[ "$bucket" =~ ^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$ ]] || { echo "invalid bucket name: $bucket" >&2; exit 2; }
[[ "$region" =~ ^[a-z0-9-]+$ ]] || { echo "invalid region: $region" >&2; exit 2; }

policy_api() {
  cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {"Sid": "BucketListForJanitorAndNotFound", "Effect": "Allow", "Action": ["s3:ListBucket"],
     "Resource": ["arn:aws:s3:::$bucket"]},
    {"Sid": "InputsUploadStatCleanup", "Effect": "Allow",
     "Action": ["s3:PutObject", "s3:GetObject", "s3:DeleteObject", "s3:AbortMultipartUpload"],
     "Resource": ["arn:aws:s3:::$bucket/in/*"]},
    {"Sid": "OutputsStatPresignedGetCleanup", "Effect": "Allow", "Action": ["s3:GetObject", "s3:DeleteObject"],
     "Resource": ["arn:aws:s3:::$bucket/out/*"]}
  ]
}
EOF
}
policy_worker() {
  cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {"Sid": "BucketListForNotFound", "Effect": "Allow", "Action": ["s3:ListBucket"],
     "Resource": ["arn:aws:s3:::$bucket"]},
    {"Sid": "InputsReadRemove", "Effect": "Allow", "Action": ["s3:GetObject", "s3:DeleteObject"],
     "Resource": ["arn:aws:s3:::$bucket/in/*"]},
    {"Sid": "OutputsWriteStatRemove", "Effect": "Allow",
     "Action": ["s3:PutObject", "s3:GetObject", "s3:DeleteObject", "s3:AbortMultipartUpload"],
     "Resource": ["arn:aws:s3:::$bucket/out/*"]}
  ]
}
EOF
}
print_policies() {
  echo "# Least-privilege policy for the api credential (CONVERTFLOW_S3_ACCESS_KEY_FILE of the api):"
  policy_api
  echo "# Least-privilege policy for the worker credential (CONVERTFLOW_S3_ACCESS_KEY_FILE of the worker):"
  policy_worker
  echo "# s3:ListBucket is needed so that a missing object reads as 404 (AWS answers 403 without it); no other bucket-level right."
}
if [ "$mode" = print ]; then print_policies; exit 0; fi

# RETENTION -> whole days, rounded up, plus one.
ret=${CONVERTFLOW_RETENTION:-24h}
[[ "$ret" =~ ^([0-9]+h)?([0-9]+m)?([0-9]+s)?$ && -n "$ret" ]] || { echo "CONVERTFLOW_RETENTION must use h/m/s units (e.g. 24h), got $ret" >&2; exit 2; }
secs=0
for part in $(grep -oE '[0-9]+[hms]' <<<"$ret"); do
  n=${part%?}
  case "$part" in *h) secs=$((secs + 10#$n * 3600)) ;; *m) secs=$((secs + 10#$n * 60)) ;; *s) secs=$((secs + 10#$n)) ;; esac
done
out_days=$(( (secs + 86399) / 86400 + 1 ))

origin_re='^https?://([A-Za-z0-9.-]+|\[[0-9A-Fa-f:.]+\])(:[0-9]{1,5})?$'
url=${S3_ADMIN_URL:-http://127.0.0.1:${CONVERTFLOW_S3_PORT:-9000}}
url=${url%/}
[[ "$url" =~ $origin_re ]] || { echo "S3_ADMIN_URL must be http(s)://host[:port], got $url" >&2; exit 2; }
origins=${CONVERTFLOW_S3_CORS_ORIGINS:-http://127.0.0.1:${CONVERTFLOW_PORT:-8080}}
cors="<CORSConfiguration><CORSRule>"
IFS=, read -r -a olist <<<"$origins"
for o in "${olist[@]}"; do
  [[ "$o" =~ $origin_re ]] || { echo "CONVERTFLOW_S3_CORS_ORIGINS entries must be http(s)://host[:port] (no wildcard), got $o" >&2; exit 2; }
  cors+="<AllowedOrigin>$o</AllowedOrigin>"
done
cors+="<AllowedMethod>POST</AllowedMethod><AllowedMethod>GET</AllowedMethod><MaxAgeSeconds>600</MaxAgeSeconds></CORSRule></CORSConfiguration>"
lifecycle=$(printf '{"Rules":[{"ID":"convertflow-in","Status":"Enabled","Filter":{"Prefix":"in/"},"Expiration":{"Days":2},"AbortIncompleteMultipartUpload":{"DaysAfterInitiation":1}},{"ID":"convertflow-out","Status":"Enabled","Filter":{"Prefix":"out/"},"Expiration":{"Days":%d},"AbortIncompleteMultipartUpload":{"DaysAfterInitiation":1}}]}' "$out_days")

secret() { local f=$1; [ -s "$f" ] || { echo "missing or empty credential file: $f" >&2; exit 1; }; tr -d '\r\n' <"$f"; }
admin_user=$(secret "${S3_ADMIN_USER_FILE:-secrets/s3_root_user}")
admin_pw=$(secret "${S3_ADMIN_PASSWORD_FILE:-secrets/s3_root_password}")
if [ "$users" = 1 ]; then
  api_ak=$(secret secrets/s3_api_access_key) api_sk=$(secret secrets/s3_api_secret_key)
  wk_ak=$(secret secrets/s3_worker_access_key) wk_sk=$(secret secrets/s3_worker_secret_key)
fi

# The program run by bash inside the client container. Values are quoted with %q; secrets reach mc
# through `mc alias import` and `mc admin user add` on stdin (both read from a pipe fed by printf, a
# bash builtin), so they never appear in a process argument list.
prog() {
  echo 'set -euo pipefail; export HOME=/tmp MC_CONFIG_DIR=/tmp/mc; mkdir -p /tmp/mc'
  printf 'printf %%s %q | mc alias import cf >/dev/null\n' \
    "{\"url\":\"$url\",\"accessKey\":\"$admin_user\",\"secretKey\":\"$admin_pw\",\"api\":\"s3v4\",\"path\":\"auto\"}"
  printf 'mc mb --ignore-existing --region %q cf/%q\n' "$region" "$bucket"
  printf 'printf %%s %q >/tmp/cors.xml\n' "$cors"
  printf 'if out=$(mc cors set cf/%q /tmp/cors.xml 2>&1); then echo "CORS rule set (origins: %s)"; ' "$bucket" "$origins"
  printf 'elif [[ ${out,,} == *"not implemented"* ]]; then echo "WARN: the store does not implement bucket CORS (MinIO): set MINIO_API_CORS_ALLOW_ORIGIN=%s on the server"; ' "$origins"
  printf 'else echo "$out" >&2; exit 1; fi\n'
  printf 'printf %%s %q | mc ilm import cf/%q >/dev/null && echo "lifecycle: in/ 2 days, out/ %d days"\n' "$lifecycle" "$bucket" "$out_days"
  if [ "$users" = 1 ]; then
    printf 'printf %%s %q >/tmp/api.json; printf %%s %q >/tmp/worker.json\n' "$(policy_api)" "$(policy_worker)"
    echo 'mc admin policy create cf convertflow-api /tmp/api.json >/dev/null'
    echo 'mc admin policy create cf convertflow-worker /tmp/worker.json >/dev/null'
    printf 'printf "%%s\\n%%s\\n" %q %q | mc admin user add cf >/dev/null\n' "$api_ak" "$api_sk"
    printf 'printf "%%s\\n%%s\\n" %q %q | mc admin user add cf >/dev/null\n' "$wk_ak" "$wk_sk"
    printf 'mc admin policy attach cf convertflow-api --user %q >/dev/null || [[ $(mc admin user info cf %q) == *convertflow-api* ]]\n' "$api_ak" "$api_ak"
    printf 'mc admin policy attach cf convertflow-worker --user %q >/dev/null || [[ $(mc admin user info cf %q) == *convertflow-worker* ]]\n' "$wk_ak" "$wk_ak"
    echo 'echo "MinIO users for api and worker created with the policies below"'
  fi
}
# The client is retried while the store is still starting.
for i in 1 2 3 4 5 6 7 8 9 10; do
  if prog | $RT run -i --rm --network host --read-only --tmpfs /tmp:size=16m --cap-drop ALL \
       --security-opt no-new-privileges --entrypoint /bin/bash "$MC_IMAGE" -s; then
    break
  fi
  [ "$i" = 10 ] && { echo "s3 bootstrap failed" >&2; exit 1; }
  echo "store not ready or bootstrap failed; retrying ($i)" >&2
  sleep 3
done
echo "bucket $bucket is ready"
print_policies
