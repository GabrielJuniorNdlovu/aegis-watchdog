#!/usr/bin/env bash
set -euo pipefail
if (($#)); then echo "Usage: bash scripts/smoke-actions.sh" >&2; exit 2; fi
cd "$(dirname "$0")/.."
command -v go >/dev/null
# Test-only SQLite fixtures and a fake Docker API. No real Docker mutations.
go test -count=1 -v ./cmd/aegis -run '^TestSimulatedActionAcknowledgmentDemo$'
