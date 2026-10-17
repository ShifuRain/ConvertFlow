#!/usr/bin/env bash
# Source-level security gates (versions pinned in scripts/tool-versions.env). Needs network for the Go module proxy
# and the vulnerability database. Requires the UI to be built first (`make ui`) because the API embeds it.
#   govulncheck  known-vulnerable dependencies/stdlib that are actually reachable
#   gosec        static analysis (reviewed false positives carry a justified `#nosec` annotation in the code)
#   licenses     permissive-only policy for Go dependencies (scripts/check-licenses.sh)
set -euo pipefail
cd "$(dirname "$0")/.."
. scripts/tool-versions.env
go run "golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION}" ./...
go run "github.com/securego/gosec/v2/cmd/gosec@${GOSEC_VERSION}" -quiet ./...
scripts/check-licenses.sh
