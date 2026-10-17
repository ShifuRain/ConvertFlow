#!/usr/bin/env bash
# Creates ./secrets with a random database password and the matching connection URL.
# The files are mounted into the containers as file secrets (/run/secrets/*); the password is never
# placed in an environment variable.
set -euo pipefail
cd "$(dirname "$0")/.."
umask 077
mkdir -p secrets
chmod 700 secrets
if [ -f secrets/db_password ] && [ -f secrets/database_url ]; then
  echo "secrets/ already exists; refusing to overwrite"; exit 0
fi
pw=$(head -c 24 /dev/urandom | base64 | tr -d '/+=\n')
printf '%s' "$pw" > secrets/db_password
printf 'postgres://convertflow:%s@db:5432/convertflow?sslmode=disable' "$pw" > secrets/database_url
# The postgres container runs as uid 999 and the apps as 65532, so the files must be readable by
# "other" inside the bind mount; this is acceptable only because ./secrets itself is mode 700 on a
# host you control. Do not relax the directory mode.
chmod 644 secrets/db_password secrets/database_url
echo "secrets written to ./secrets"
