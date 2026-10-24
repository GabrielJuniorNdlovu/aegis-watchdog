#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
task_dir="$(mktemp -d "${TMPDIR:-/tmp}/aegis-console-smoke.XXXXXXXX")"
trap 'rm -rf -- "$task_dir"' EXIT
CGO_ENABLED=1 go build -o "$task_dir/aegis" ./cmd/aegis
# This test uses only its own PTYs, SQLite journal, and fake Docker HTTP API.
timeout --signal=TERM --kill-after=5s 90s python3 scripts/console-smoke.py "$task_dir/aegis" "$task_dir"
