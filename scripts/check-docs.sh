#!/usr/bin/env bash
# Documentation consistency checks (needs python3, nothing else):
#   (a) every relative link and #anchor in README.md, SECURITY.md, CONTRIBUTING.md and docs/*.md resolves
#   (b) every CONVERTFLOW_* variable read by internal/config/config.go has a row in the configuration table ("### Variables")
#       of docs/operations.md (a mention elsewhere does not count), and every
#       CONVERTFLOW_* name used anywhere in those documents exists (config.go, or a ${CONVERTFLOW_...} variable of compose.yaml)
#   (c) every route (method + path) of the OpenAPI document internal/api/openapi.json appears in a heading or a code span of docs/api.md
# Usage: scripts/check-docs.sh [--self-test]
set -euo pipefail
cd "$(dirname "$0")/.."
if [ "${1:-}" = "--self-test" ]; then
  # Run the checker against a copy with deliberately broken docs; every defect must be reported.
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  mkdir -p "$tmp/docs" "$tmp/internal/config" "$tmp/internal/api"
  cp internal/config/config.go "$tmp/internal/config/"; cp internal/api/openapi.json "$tmp/internal/api/"; cp compose.yaml "$tmp/"
  printf '# T\n[ok](docs/a.md#sec) [bad file](docs/missing.md) [bad anchor](docs/a.md#nope)\n' > "$tmp/README.md"
  printf '# A\n## Sec\nvars: `CONVERTFLOW_LISTEN` `CONVERTFLOW_DOES_NOT_EXIST`\n' > "$tmp/docs/a.md"
  # CONVERTFLOW_MAX_PIXELS is mentioned in prose and in a code span outside the table only; LISTEN has a table row.
  printf '# Ops\n`CONVERTFLOW_MAX_PIXELS` is described here.\n### Variables\n| Variable | Default |\n|---|---|\n| `CONVERTFLOW_LISTEN` | x |\n### Other\n| `CONVERTFLOW_MAX_DURATION_SEC` | x |\n' > "$tmp/docs/operations.md"
  # GET /healthz is in a code span (ok); DELETE route is plain prose (must be reported); POST route is in a heading (ok).
  printf '# API\n`GET /healthz`\nDELETE /api/v1/jobs/{id} is described in prose only.\n## POST /api/v1/jobs\n' > "$tmp/docs/api.md"
  out=$(CHECK_DOCS_ROOT="$tmp" "$0" 2>&1) && rc=0 || rc=$?
  bad=0
  [ "$rc" -ne 0 ] || { echo "SELF-TEST FAIL: broken docs passed"; bad=1; }
  for want in "missing.md" "#nope" "CONVERTFLOW_DOES_NOT_EXIST" "CONVERTFLOW_MAX_UPLOAD_BYTES" \
    "not in the configuration table of docs/operations.md: CONVERTFLOW_MAX_PIXELS" \
    "not in the configuration table of docs/operations.md: CONVERTFLOW_MAX_DURATION_SEC" \
    "not in a heading or code span: DELETE /api/v1/jobs/{id}"; do
    grep -qF "$want" <<<"$out" || { echo "SELF-TEST FAIL: expected report missing: $want"; bad=1; }
  done
  # things that are correctly documented must NOT be reported
  for nowant in "table of docs/operations.md: CONVERTFLOW_LISTEN" "code span: GET /healthz" "code span: POST /api/v1/jobs"; do
    # whole-line suffix match: "POST /api/v1/jobs" must not match the report for "POST /api/v1/jobs/{id}/complete"
    ! awk -v s="$nowant" 'length($0) >= length(s) && substr($0, length($0) - length(s) + 1) == s { f = 1 } END { exit !f }' <<<"$out" \
      || { echo "SELF-TEST FAIL: false positive: $nowant"; bad=1; }
  done
  [ "$bad" = 0 ] && echo "self-test ok: all seeded defects were reported"
  exit "$bad"
fi
exec python3 - "${CHECK_DOCS_ROOT:-.}" <<'PY'
import json, os, re, sys

root = sys.argv[1]
errors = []

def read(p):
    with open(os.path.join(root, p), encoding="utf-8") as f:
        return f.read()

docs = [p for p in ("README.md", "SECURITY.md", "CONTRIBUTING.md") if os.path.exists(os.path.join(root, p))]
ddir = os.path.join(root, "docs")
if os.path.isdir(ddir):
    docs += sorted("docs/" + f for f in os.listdir(ddir) if f.endswith(".md"))

def strip_code(text):
    text = re.sub(r"^(```|~~~).*?^\1[ \t]*$", "", text, flags=re.S | re.M)   # fenced blocks
    return re.sub(r"`[^`\n]*`", "", text)                                      # inline code

def slug(h):
    h = re.sub(r"`", "", h.strip().lower())
    h = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", h)
    h = re.sub(r"[^\w\- ]", "", h, flags=re.U)
    return h.replace(" ", "-")

anchors = {}
def anchors_of(p):
    if p not in anchors:
        text = read(p)
        a, seen = set(), {}
        body = re.sub(r"^(```|~~~).*?^\1[ \t]*$", "", text, flags=re.S | re.M)
        for m in re.finditer(r"^#{1,6}[ \t]+(.+?)[ \t#]*$", body, flags=re.M):
            s = slug(m.group(1))
            n = seen.get(s, 0)
            seen[s] = n + 1
            a.add(s if n == 0 else f"{s}-{n}")
        a |= set(re.findall(r'<a\s+(?:id|name)="([^"]+)"', text))
        anchors[p] = a
    return anchors[p]

# (a) links
for d in docs:
    text = strip_code(read(d))
    for m in re.finditer(r"(?<!\!)\[[^\]]*\]\(([^)\s]+)(?:\s+\"[^\"]*\")?\)", text):
        target = m.group(1)
        if re.match(r"^[a-z][a-z0-9+.-]*:", target) or target.startswith("//"):
            continue
        path, _, frag = target.partition("#")
        base = os.path.dirname(d)
        rel = os.path.normpath(os.path.join(base, path)) if path else d
        if path and not os.path.exists(os.path.join(root, rel)):
            errors.append(f"{d}: link target not found: {target}")
            continue
        if frag and rel.endswith(".md") and frag not in anchors_of(rel):
            errors.append(f"{d}: anchor not found: {target}")

# (b) environment variables
cfg = read("internal/config/config.go")
code_vars = set(re.findall(r'"(CONVERTFLOW_[A-Z][A-Z0-9_]*)"', cfg))
compose_vars = set(re.findall(r"\$\{(CONVERTFLOW_[A-Z][A-Z0-9_]*)", read("compose.yaml")))
ops = read("docs/operations.md") if os.path.exists(os.path.join(root, "docs/operations.md")) else ""
# only rows of the table under "### Variables" count: `| `CONVERTFLOW_X` | ... |`
sec = re.search(r"^###[ \t]+Variables[ \t]*$(.*?)(?=^#{1,3}[ \t]|\Z)", ops, flags=re.S | re.M)
table_vars = set(re.findall(r"^\|[ \t]*`(CONVERTFLOW_[A-Z][A-Z0-9_]*)`[ \t]*\|", sec.group(1) if sec else "", flags=re.M))
for v in sorted(code_vars):
    if v not in table_vars:
        errors.append(f"variable read by config.go is not in the configuration table of docs/operations.md: {v}")
known = code_vars | compose_vars
for d in docs:
    for v in sorted(set(re.findall(r"\bCONVERTFLOW_[A-Z][A-Z0-9_]*\b", read(d)))):
        if v not in known:
            errors.append(f"{d}: documents a variable that does not exist: {v}")

# (c) API routes
api = read("docs/api.md") if os.path.exists(os.path.join(root, "docs/api.md")) else ""
spec = json.loads(read("internal/api/openapi.json"))
# a route counts when a heading line contains it or an inline code span equals it (fenced blocks do not count)
api_body = re.sub(r"^(```|~~~).*?^\1[ \t]*$", "", api, flags=re.S | re.M)
api_refs = set()
for line in api_body.splitlines():
    spans = [c.strip() for c in re.findall(r"`([^`\n]+)`", line)]
    api_refs.update(spans)
    if re.match(r"^#{1,6}[ \t]", line):
        for m in re.finditer(r"\b(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS) (/[^\s`]*)", line):
            api_refs.add(f"{m.group(1)} {m.group(2)}")
for path, ops_ in spec.get("paths", {}).items():
    for method in ops_:
        if method.upper() not in ("GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"):
            continue
        route = f"{method.upper()} {path}"
        if route not in api_refs:
            errors.append(f"docs/api.md: route not in a heading or code span: {route}")

if errors:
    print("\n".join(sorted(set(errors))))
    print(f"check-docs: {len(set(errors))} problem(s)")
    sys.exit(1)
print(f"check-docs: ok ({len(docs)} documents, {len(code_vars)} config variables, routes from openapi.json)")
PY
