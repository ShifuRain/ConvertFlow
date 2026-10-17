#!/bin/sh
# Runs once, on an EMPTY data directory only, from the official postgres image entrypoint
# (/docker-entrypoint-initdb.d), as the bootstrap superuser over the unix socket.
#
# Creates the least-privilege application role and the application database it owns. The password
# is read from a file secret (/run/secrets/db_app_password) by psql itself (backtick in \set), so it
# never appears in an environment variable, a process argument list or an image layer.
# Existing data directories are NOT changed by this script: see docs/operations.md (upgrade note).
set -eu
: "${POSTGRES_USER:?}"
pwfile=/run/secrets/db_app_password
[ -s "$pwfile" ] || { echo "db-init: $pwfile missing or empty (run scripts/gen-secrets.sh)" >&2; exit 1; }

psql -v ON_ERROR_STOP=1 --no-psqlrc --username "$POSTGRES_USER" --dbname postgres <<'SQL'
\set app_pw `cat /run/secrets/db_app_password`
CREATE ROLE convertflow_app LOGIN
  NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS
  PASSWORD :'app_pw';
CREATE DATABASE convertflow OWNER convertflow_app;
REVOKE ALL ON DATABASE convertflow FROM PUBLIC;
\connect convertflow
-- PostgreSQL 15+: the public schema is owned by pg_database_owner (= convertflow_app) and PUBLIC may
-- not create objects in it. Make that explicit so it does not depend on the server version.
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT ALL ON SCHEMA public TO convertflow_app;
SQL
