#!/usr/bin/env bash
# Enforces the .trivyignore policy: every entry needs "# justification: ..." and "# expires: YYYY-MM-DD" comment lines
# directly above it, the expiry must not be in the past and must not be more than 90 days away.
# Usage: scripts/check-trivyignore.sh [file]      (default .trivyignore; TODAY=YYYY-MM-DD overrides the date for tests)
#        scripts/check-trivyignore.sh --self-test
set -euo pipefail
cd "$(dirname "$0")/.."

check() {
  local file=$1 today=${TODAY:-$(date -u +%F)} rc=0 just="" exp="" n=0 line
  local max; max=$(date -u -d "$today + 90 days" +%F)
  while IFS= read -r line || [ -n "$line" ]; do
    n=$((n+1))
    case "$line" in
      "# justification: "?*) just=$line ;;
      "# expires: "*) exp=${line#"# expires: "} ;;
      "#"*) ;;
      "") just="" exp="" ;;                       # a blank line detaches comments from the next entry
      *)
        id=${line%%[[:space:]]*}
        if [ -z "$just" ]; then echo "$file:$n: $id has no '# justification:' line directly above"; rc=1; fi
        if ! [[ "$exp" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] || ! date -u -d "$exp" +%F >/dev/null 2>&1; then
          echo "$file:$n: $id has no valid '# expires: YYYY-MM-DD' line directly above"; rc=1
        elif [[ "$exp" < "$today" ]]; then echo "$file:$n: $id EXPIRED on $exp - fix the finding or renew with a new justification"; rc=1
        elif [[ "$exp" > "$max" ]]; then echo "$file:$n: $id expires $exp, more than 90 days away (max $max)"; rc=1
        fi
        just="" exp="" ;;
    esac
  done < "$file"
  return $rc
}

if [ "${1:-}" = "--self-test" ]; then
  t=$(mktemp -d); trap 'rm -rf "$t"' EXIT; bad=0
  printf '# justification: no fix path, library not reachable\n# expires: 2026-12-01\nCVE-2026-0001\n' > "$t/ok"
  printf 'CVE-2026-0002\n' > "$t/nojust"
  printf '# justification: x\nCVE-2026-0003\n' > "$t/noexp"
  printf '# justification: x\n# expires: 2026-01-01\nCVE-2026-0004\n' > "$t/expired"
  printf '# justification: x\n# expires: 2030-01-01\nCVE-2026-0005\n' > "$t/toofar"
  printf '# justification: x\n# expires: 2026-12-01\n\nCVE-2026-0006\n' > "$t/detached"
  export TODAY=2026-10-17
  check "$t/ok" >/dev/null && echo "self-test ok: valid entry accepted" || { echo "SELF-TEST FAIL: valid entry rejected"; bad=1; }
  for f in nojust noexp expired toofar detached; do
    check "$t/$f" >/dev/null && { echo "SELF-TEST FAIL: $f accepted"; bad=1; } || echo "self-test ok: $f rejected"
  done
  exit $bad
fi
f=${1:-.trivyignore}
[ -f "$f" ] || { echo "$f not found" >&2; exit 2; }
check "$f" && echo "$f ok"
