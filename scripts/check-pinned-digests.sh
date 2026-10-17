#!/usr/bin/env bash
# Supply-chain pinning checks.
#
# Static (always, offline, FAILS on violation):
#   * every FROM in Dockerfile.* is <ref>@sha256:<64 hex> and is not :latest (stage references and scratch excepted)
#   * every external `image:` in compose.yaml is digest-pinned (the project's own convertflow-* images excepted)
#   * every `uses:` in .github/workflows is pinned to a 40-hex commit SHA (local ./ actions excepted)
#   * every container image referenced by workflows or scripts/tool-versions.env is digest-pinned
#   * no `:latest` / `@latest` anywhere in Dockerfiles, compose, workflows, scripts
# Online (--online, needs network; uses crane from the pinned module, no container runtime needed):
#   * the pinned digest still resolves in the registry                    -> FAIL if not (image was deleted/rewritten)
#   * the pinned digest is still the CURRENT digest of its tag            -> WARN (exit 0); FAIL with --strict
#     A moved tag usually just means upstream published a patch rebuild: run scripts/pin-digests.sh, review, commit.
# Policy: unresolvable digest = hard failure; stale digest = warning (the weekly workflow surfaces the warning and
# --strict is available for release gating). Usage: scripts/check-pinned-digests.sh [--online] [--strict]
set -euo pipefail
cd "$(dirname "$0")/.."
. scripts/tool-versions.env
online=0 strict=0
for a in "$@"; do case "$a" in --online) online=1 ;; --strict) strict=1 ;; *) echo "unknown option $a" >&2; exit 2 ;; esac; done

fail=0
bad() { echo "FAIL: $*"; fail=1; }
digest_re='@sha256:[0-9a-f]{64}$'

# ---- Dockerfiles -------------------------------------------------------------------------------
for f in Dockerfile.*; do
  [ -f "$f" ] || continue
  stages=" "
  while read -r ref name; do
    case "$stages" in *" $ref "*) ;; *)   # FROM <earlier stage> needs no digest
      if [ "$ref" != scratch ]; then
        [[ "$ref" =~ $digest_re ]] || bad "$f: FROM $ref is not pinned by digest"
        [[ "$ref" == *:latest* ]] && bad "$f: FROM $ref uses :latest"
      fi ;;
    esac
    [ -n "$name" ] && stages+="$name "
  done < <(awk 'toupper($1)=="FROM" { i=2; if ($i ~ /^--/) i++; n=""; if (toupper($(i+1))=="AS") n=$(i+2); print $i, n }' "$f")
done

# ---- compose -----------------------------------------------------------------------------------
while read -r ref; do
  case "$ref" in convertflow-*) continue ;; esac
  [[ "$ref" =~ $digest_re ]] || bad "compose.yaml: image $ref is not pinned by digest"
done < <(grep -E '^[[:space:]]+image:' compose.yaml | sed -E 's/^[[:space:]]+image:[[:space:]]*//; s/["'\'']//g')

# ---- workflows ---------------------------------------------------------------------------------
shopt -s nullglob
wf=(.github/workflows/*.yml .github/workflows/*.yaml)
for f in "${wf[@]}"; do
  n=0
  while IFS= read -r line; do
    n=$((n+1))
    ref=$(sed -E 's/^[[:space:]]*-?[[:space:]]*uses:[[:space:]]*//; s/[[:space:]]+#.*$//; s/["'\'']//g' <<<"$line")
    case "$ref" in ./*) continue ;; esac
    [[ "$ref" =~ @[0-9a-f]{40}$ ]] || bad "$f: action '$ref' is not pinned to a full commit SHA"
  done < <(grep -E '^[[:space:]]*-?[[:space:]]*uses:' "$f" || true)
  while IFS= read -r ref; do
    [[ "$ref" =~ $digest_re ]] || bad "$f: container image '$ref' is not pinned by digest"
  done < <(grep -E '^[[:space:]]+image:[[:space:]]' "$f" | sed -E 's/^[[:space:]]+image:[[:space:]]*//; s/[[:space:]]+#.*$//; s/["'\'']//g' || true)
done
for v in TRIVY_IMAGE SYFT_IMAGE; do
  [[ "${!v}" =~ $digest_re ]] || bad "scripts/tool-versions.env: $v is not pinned by digest"
done

# ---- no :latest / @latest anywhere ---------------------------------------------------------------
if grep -nE '(:|@)latest([^A-Za-z0-9_-]|$)' Dockerfile.* compose.yaml "${wf[@]}" scripts/*.sh scripts/*.env 2>/dev/null \
   | grep -v 'check-pinned-digests.sh'; then
  bad "':latest' or '@latest' found (see lines above)"
fi

# ---- online staleness ----------------------------------------------------------------------------
warn=0
if [ "$online" = 1 ]; then
  crane() { go run "github.com/google/go-containerregistry/cmd/crane@${CRANE_VERSION}" "$@"; }
  refs=$( { grep -hoE '[a-z0-9.-]+(:[0-9]+)?(/[a-z0-9._-]+)+:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}' Dockerfile.* compose.yaml "${wf[@]}" scripts/tool-versions.env 2>/dev/null || true; } | sort -u)
  for full in $refs; do
    tagref=${full%@*} pinned=${full#*@}
    if ! crane manifest "$full" >/dev/null 2>&1; then bad "$full no longer resolves in its registry"; continue; fi
    cur=$(crane digest "$tagref" 2>/dev/null || true)
    if [ -z "$cur" ]; then bad "$tagref: could not resolve the current digest of the tag"; continue; fi
    if [ "$cur" = "$pinned" ]; then echo "ok:   $tagref is current ($pinned)"
    else
      msg="$tagref moved: pinned $pinned, registry now $cur (base images: run scripts/pin-digests.sh; tool images: edit scripts/tool-versions.env; review, commit)"
      if [ "$strict" = 1 ]; then bad "$msg"; else echo "WARN: $msg"; echo "::warning title=stale base image digest::$msg"; warn=1; fi
    fi
  done
fi

if [ "$fail" = 0 ]; then echo "pinning checks passed"; [ "$warn" = 0 ] || echo "(with stale-digest warnings, see above)"; fi
exit "$fail"
