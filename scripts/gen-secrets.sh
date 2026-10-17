#!/usr/bin/env bash
# Creates ./secrets with random database passwords and the application's connection URL.
#   db_password      bootstrap superuser (convertflow_admin); only the db container's entrypoint uses it
#   db_app_password  application role (convertflow_app, no superuser rights); read by docker/db-init at first start
#   database_url     connection URL for api and worker, using convertflow_app
# The files are mounted into the containers as file secrets (/run/secrets/*); passwords are never
# placed in an environment variable.
set -euo pipefail
cd "$(dirname "$0")/.."
umask 077
mkdir -p secrets
chmod 700 secrets
files=(db_password db_app_password database_url)
have=0
for f in "${files[@]}"; do [ -f "secrets/$f" ] && have=$((have + 1)); done
if [ "$have" -eq "${#files[@]}" ]; then
  echo "secrets/ already exists; refusing to overwrite"; exit 0
fi
if [ "$have" -ne 0 ]; then
  echo "secrets/ is incomplete or from an older layout (needs: ${files[*]})." >&2
  echo "An existing database volume keeps its old role: follow the upgrade note in docs/operations.md." >&2
  exit 1
fi
gen() { head -c 24 /dev/urandom | base64 | tr -d '/+=\n'; }
admin_pw=$(gen)
app_pw=$(gen)
printf '%s' "$admin_pw" > secrets/db_password
printf '%s' "$app_pw" > secrets/db_app_password
printf 'postgres://convertflow_app:%s@db:5432/convertflow?sslmode=disable' "$app_pw" > secrets/database_url
# The postgres container runs as uid 999 and the apps as 65532, so the files must be readable by
# "other" inside the bind mount; this is acceptable only because ./secrets itself is mode 700 on a
# host you control. Do not relax the directory mode.
chmod 644 secrets/db_password secrets/db_app_password secrets/database_url
echo "secrets written to ./secrets"
