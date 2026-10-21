#!/usr/bin/env bash
set -euo pipefail
if (($#)); then echo "Usage: bash scripts/smoke-evidence.sh" >&2; exit 2; fi
cd "$(dirname "$0")/.."
command -v go >/dev/null
docker compose version >/dev/null
if [[ -z ${DOCKER_HOST:-} ]]; then
  export DOCKER_HOST
  DOCKER_HOST=$(docker context inspect --format '{{.Endpoints.docker.Host}}')
fi
unset DOCKER_CONTEXT
docker version --format '{{.Server.Version}}' >/dev/null

temp_dir=$(mktemp -d "${TMPDIR:-/tmp}/aegis-evidence.XXXXXXXX")
project="aegis-evidence-$(basename "$temp_dir" | tr '[:upper:].' '[:lower:]-')"
compose=(docker compose -f demo/compose.yaml -f demo/evidence.compose.yaml -p "$project")
cleanup() {
  local status=$?
  trap - EXIT
  "${compose[@]}" down --timeout 5 >&2 || status=1
  rm -rf -- "$temp_dir"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
"${compose[@]}" up -d --wait --wait-timeout 60 healthy
container_id=$("${compose[@]}" ps -q healthy)
[[ $container_id =~ ^[a-f0-9]{64}$ ]] || { echo 'Missing isolated container ID' >&2; exit 1; }
restart_before=$(docker inspect --format '{{.RestartCount}}' "$container_id")
mkdir -m 700 "$temp_dir/data"
echo 'SIMULATED actions only; private disposable database; synthetic sandbox logs.'
export AEGIS_EVIDENCE_DEMO_CONTAINER="$container_id"
export AEGIS_EVIDENCE_DEMO_DIR="$temp_dir/data"
go test -count=1 -v ./internal/evidence -run '^TestLiveEvidenceDemo$'
go build -o "$temp_dir/aegis" ./cmd/aegis
action_output=$(DOCKER_HOST=invalid-Docker-host "$temp_dir/aegis" actions --id 1 --db "$temp_dir/data/simulated-actions.db")
grep -Fxq 'Evidence status: complete' <<<"$action_output"
if grep -Fq 'synthetic evidence demo' <<<"$action_output"; then
  echo 'Action inspection printed full logs' >&2
  exit 1
fi
printf '%s\n' "$action_output"
restart_after=$(docker inspect --format '{{.RestartCount}}' "$container_id")
[[ $restart_after == "$restart_before" ]] || { echo 'Restart count changed' >&2; exit 1; }
echo 'PASS: action inspection without Docker; restart count unchanged; automatic scoped cleanup on exit.'
