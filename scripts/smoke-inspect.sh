#!/usr/bin/env bash
set -euo pipefail

if (($#)); then
  echo "Usage: bash scripts/smoke-inspect.sh" >&2
  exit 2
fi
cd "$(dirname "$0")/.."
command -v go >/dev/null
docker compose version >/dev/null
# The SDK reads DOCKER_HOST, while the Docker CLI also understands contexts.
if [[ -z ${DOCKER_HOST:-} ]]; then
  export DOCKER_HOST
  DOCKER_HOST=$(docker context inspect --format '{{.Endpoints.docker.Host}}')
fi
unset DOCKER_CONTEXT
docker version --format '{{.Server.Version}}' >/dev/null

temp_dir=$(mktemp -d "${TMPDIR:-/tmp}/aegis-smoke.XXXXXXXX")
project="aegis-smoke-$(basename "$temp_dir" | tr '[:upper:].' '[:lower:]-')"
compose=(docker compose -f demo/compose.yaml -p "$project")
cleanup() {
  local status=$?
  trap - EXIT
  "${compose[@]}" down --timeout 5 --remove-orphans >&2 || status=1
  rm -rf -- "$temp_dir"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

go build -o "$temp_dir/aegis" ./cmd/aegis
"${compose[@]}" up -d
deadline=$((SECONDS + 60))
for pair in healthy:healthy unhealthy:unhealthy no-healthcheck:unknown; do
  service=${pair%:*}
  expected=${pair#*:}
  container_id=$("${compose[@]}" ps -q "$service")
  [[ -n $container_id ]] || { echo "Missing demo container: $service" >&2; exit 1; }
  while :; do
    output=$("$temp_dir/aegis" inspect --container "$container_id")
    if grep -Fxq "Health: $expected" <<<"$output"; then break; fi
    if ((SECONDS >= deadline)); then
      echo "Timed out waiting for $service to report $expected" >&2
      printf '%s\n' "$output" >&2
      exit 1
    fi
    sleep 1
  done
  grep -Fxq "Docker ID: $container_id" <<<"$output"
  grep -Fxq 'Lifecycle: running' <<<"$output"
  grep -Fxq 'Action: none — inspection only.' <<<"$output"
  if [[ $expected != unknown ]]; then
    grep -Eq '^Last completed health check: .+Z \(age: ' <<<"$output"
  else
    grep -Fxq 'Last completed health check: unavailable' <<<"$output"
  fi
  printf 'PASS: %s reports %s with exit 0\n' "$service" "$expected"
done
