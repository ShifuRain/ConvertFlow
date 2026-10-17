#!/usr/bin/env bash
# License policy for Go dependencies (spec: permissive only). Fails on anything that is not one of the
# allowed SPDX licenses, which excludes GPL/AGPL/LGPL and also MPL-2.0 (weak copyleft; no dependency needs it,
# so it is not allowed - add it here with a written justification if that ever changes).
# The UI toolchain (esbuild, typescript) is build-time only and is not part of any shipped artifact.
set -euo pipefail
cd "$(dirname "$0")/.."
. scripts/tool-versions.env
ALLOWED=MIT,BSD-2-Clause,BSD-3-Clause,Apache-2.0,ISC
go run "github.com/google/go-licenses@${GO_LICENSES_VERSION}" check ./... --allowed_licenses="$ALLOWED"
echo "license policy ok (allowed: $ALLOWED)"
