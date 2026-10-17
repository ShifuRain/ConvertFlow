#!/usr/bin/env bash
# Runs the full test suite against a throwaway Postgres container.
set -euo pipefail
RT=${CONTAINER_RUNTIME:-docker}
NAME=cf-test-pg-$$
$RT run --rm -d --name "$NAME" -e POSTGRES_PASSWORD=test -p 127.0.0.1::5432 postgres:16-bookworm >/dev/null
trap '$RT stop "$NAME" >/dev/null' EXIT
for _ in $(seq 1 60); do
  $RT exec "$NAME" pg_isready -U postgres >/dev/null 2>&1 && break
  sleep 1
done
PORT=$($RT port "$NAME" 5432/tcp | head -n1 | sed 's/.*://')
export TEST_DATABASE_URL="postgres://postgres:test@127.0.0.1:${PORT}/postgres?sslmode=disable"
go vet ./...
go test ./... "$@"
