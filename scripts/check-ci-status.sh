#!/usr/bin/env bash
# Release gate: the CI workflow must have SUCCEEDED for the exact commit being released. Fails closed: no run, an
# unfinished run, or a non-success conclusion all fail. Reads the JSON of
#   gh api "repos/<owner>/<repo>/actions/workflows/ci.yml/runs?head_sha=<sha>&per_page=100"
# from a file (so the logic can be tested with canned data), and takes the most recent run for that SHA.
# Usage: scripts/check-ci-status.sh <runs.json> <sha>      |      scripts/check-ci-status.sh --self-test
set -euo pipefail

verdict() {  # <json-file> <sha>
  jq -e --arg sha "$2" '
    [(.workflow_runs // [])[] | select(.head_sha == $sha and (.path // "") == ".github/workflows/ci.yml")]
    | sort_by(.created_at) | last
    | if . == null then error("no CI run found for this commit")
      elif .status != "completed" then error("latest CI run is \(.status), not completed")
      elif .conclusion != "success" then error("latest CI run concluded \(.conclusion)")
      else "CI run \(.id) succeeded" end' "$1" >&2
}

if [ "${1:-}" = "--self-test" ]; then
  t=$(mktemp -d); trap 'rm -rf "$t"' EXIT; bad=0; sha=$(printf 'a%.0s' {1..40}); other=$(printf 'b%.0s' {1..40})
  run() { printf '{"id":%s,"head_sha":"%s","path":".github/workflows/ci.yml","status":"%s","conclusion":%s,"created_at":"%s"}' "$1" "$2" "$3" "$4" "$5"; }
  printf '{"workflow_runs":[%s]}' "$(run 1 $sha completed '"success"' 2026-01-01T00:00:00Z)" > "$t/ok"
  printf '{"workflow_runs":[%s]}' "$(run 1 $sha completed '"failure"' 2026-01-01T00:00:00Z)" > "$t/failed"
  printf '{"workflow_runs":[%s]}' "$(run 1 $sha in_progress null 2026-01-01T00:00:00Z)" > "$t/running"
  printf '{"workflow_runs":[%s]}' "$(run 1 $other completed '"success"' 2026-01-01T00:00:00Z)" > "$t/othersha"
  printf '{"workflow_runs":[]}' > "$t/none"
  printf '{"workflow_runs":[%s,%s]}' "$(run 1 $sha completed '"failure"' 2026-01-01T00:00:00Z)" "$(run 2 $sha completed '"success"' 2026-01-02T00:00:00Z)" > "$t/rerun-ok"
  printf '{"workflow_runs":[%s,%s]}' "$(run 1 $sha completed '"success"' 2026-01-01T00:00:00Z)" "$(run 2 $sha completed '"failure"' 2026-01-02T00:00:00Z)" > "$t/latest-failed"
  printf '{"workflow_runs":[%s]}' "$(run 1 $sha completed '"success"' 2026-01-01T00:00:00Z | sed 's#ci.yml#other.yml#')" > "$t/wrongworkflow"
  printf 'not json' > "$t/garbage"
  for f in ok rerun-ok; do verdict "$t/$f" $sha >/dev/null 2>&1 && echo "self-test ok: $f accepted" || { echo "SELF-TEST FAIL: $f rejected"; bad=1; }; done
  for f in failed running othersha none latest-failed wrongworkflow garbage; do
    verdict "$t/$f" $sha >/dev/null 2>&1 && { echo "SELF-TEST FAIL: $f accepted"; bad=1; } || echo "self-test ok: $f rejected"
  done
  exit $bad
fi
[ $# -eq 2 ] || { echo "usage: $0 <runs.json> <sha>" >&2; exit 2; }
verdict "$1" "$2"
