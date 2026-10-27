#!/usr/bin/env bash
# Creates ./secrets with random database passwords and the application's connection URL.
#   db_password      bootstrap superuser (convertflow_admin); only the db container's entrypoint uses it
#   db_app_password  application role (convertflow_app, no superuser rights); read by docker/db-init at first start
#   database_url     connection URL for api and worker, using convertflow_app
# and the credentials of the DEV/TEST object store profile (compose.s3.yaml; unused by compose.yaml alone):
#   s3_root_user, s3_root_password            store administrator (s3 container and scripts/s3-bootstrap.sh only)
#   s3_api_access_key, s3_api_secret_key      least-privilege store user of the api
#   s3_worker_access_key, s3_worker_secret_key least-privilege store user of the worker
# The files are mounted into the containers as file secrets (/run/secrets/*); passwords are never
# placed in an environment variable. Each group is created only when none of its files exist, so an
# existing ./secrets from before the s3 profile gains the s3 files and keeps its database files.
set -euo pipefail
cd "$(dirname "$0")/.."
umask 077
mkdir -p secrets
chmod 700 secrets
db_files=(db_password db_app_password database_url)
s3_files=(s3_root_user s3_root_password s3_api_access_key s3_api_secret_key s3_worker_access_key s3_worker_secret_key)

# count <files...>: how many of them exist
count() { local n=0 f; for f in "$@"; do [ -f "secrets/$f" ] && n=$((n + 1)); done; echo "$n"; }
gen() { head -c 24 /dev/urandom | base64 | tr -d '/+=\n'; }
# MinIO limits access keys to 20 characters and secret keys to 40.
key() { head -c 64 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c "$1"; }

db_have=$(count "${db_files[@]}")
s3_have=$(count "${s3_files[@]}")
if [ "$db_have" -ne 0 ] && [ "$db_have" -ne "${#db_files[@]}" ]; then
  echo "secrets/ is incomplete or from an older layout (needs: ${db_files[*]})." >&2
  echo "An existing database volume keeps its old role: follow the upgrade note in docs/operations.md." >&2
  exit 1
fi
if [ "$s3_have" -ne 0 ] && [ "$s3_have" -ne "${#s3_files[@]}" ]; then
  echo "secrets/ has only some of the object store files (needs: ${s3_files[*]}); remove them to regenerate." >&2
  exit 1
fi
if [ "$db_have" -eq "${#db_files[@]}" ] && [ "$s3_have" -eq "${#s3_files[@]}" ]; then
  echo "secrets/ already exists; refusing to overwrite"; exit 0
fi

if [ "$db_have" -eq 0 ]; then
  admin_pw=$(gen)
  app_pw=$(gen)
  printf '%s' "$admin_pw" > secrets/db_password
  printf '%s' "$app_pw" > secrets/db_app_password
  printf 'postgres://convertflow_app:%s@db:5432/convertflow?sslmode=disable' "$app_pw" > secrets/database_url
fi
if [ "$s3_have" -eq 0 ]; then
  printf 'cfadmin%s' "$(key 12)" > secrets/s3_root_user
  key 40 > secrets/s3_root_password
  printf 'cfapi%s' "$(key 15)" > secrets/s3_api_access_key
  key 40 > secrets/s3_api_secret_key
  printf 'cfworker%s' "$(key 12)" > secrets/s3_worker_access_key
  key 40 > secrets/s3_worker_secret_key
fi
# The postgres container runs as uid 999, the store and the apps as 65532, so the files must be
# readable by "other" inside the bind mount; this is acceptable only because ./secrets itself is
# mode 700 on a host you control. Do not relax the directory mode.
chmod 644 "${db_files[@]/#/secrets/}" "${s3_files[@]/#/secrets/}"
[ "$db_have" -eq 0 ] && echo "database secrets written to ./secrets"
[ "$s3_have" -eq 0 ] && echo "object store secrets written to ./secrets"
exit 0
