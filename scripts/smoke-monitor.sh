#!/usr/bin/env bash
set -euo pipefail

if (($#)); then echo "Usage: bash scripts/smoke-monitor.sh" >&2; exit 2; fi
cd "$(dirname "$0")/.."
command -v go >/dev/null
docker compose version >/dev/null
if [[ -z ${DOCKER_HOST:-} ]]; then
  export DOCKER_HOST
  DOCKER_HOST=$(docker context inspect --format '{{.Endpoints.docker.Host}}')
fi
unset DOCKER_CONTEXT
docker version --format '{{.Server.Version}}' >/dev/null

# Keep the bind mount under the project, which Docker Desktop can share.
mkdir -p data
temp_dir=$(mktemp -d "$PWD/data/aegis-monitor-demo.XXXXXXXX")
project=$(basename "$temp_dir" | tr '[:upper:].' '[:lower:]-')
export AEGIS_DEMO_CONTROL_DIR="$temp_dir/control"
compose=(docker compose -f demo/compose.yaml -f demo/monitor.compose.yaml -p "$project")
monitor_pid=""
cleanup() {
  local status=$?
  trap - EXIT
  if [[ -n $monitor_pid ]]; then
    kill -TERM "$monitor_pid" 2>/dev/null || true
    wait "$monitor_pid" 2>/dev/null || true
  fi
  "${compose[@]}" down --timeout 5 --remove-orphans >&2 || status=1
  rm -rf -- "$temp_dir"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

mkdir "$AEGIS_DEMO_CONTROL_DIR"
chmod 755 "$AEGIS_DEMO_CONTROL_DIR"
touch "$AEGIS_DEMO_CONTROL_DIR/healthy"
CGO_ENABLED=1 go build -o "$temp_dir/aegis" ./cmd/aegis
"${compose[@]}" up -d healthy
container_id=$("${compose[@]}" ps -q healthy)
[[ -n $container_id ]]
database="$temp_dir/data/history.db"

start_monitor() {
  "$temp_dir/aegis" monitor --container "$container_id" --interval 200ms --retention 168h --db "$database" >"$temp_dir/monitor.out" 2>"$temp_dir/monitor.err" &
  monitor_pid=$!
}
stop_monitor() {
  local pid=$monitor_pid
  monitor_pid=""
  kill -TERM "$pid"
  wait "$pid"
}
read_history() {
  DOCKER_HOST=invalid-Docker-host "$temp_dir/aegis" history --container "$container_id" --limit 1000 --db "$database"
}
wait_for_health() {
  local expected=$1 deadline=$((SECONDS + 30))
  while :; do
    if history=$(read_history 2>"$temp_dir/history.err"); then
      if [[ $(awk '/^Health: / {print $2; exit}' <<<"$history") == "$expected" ]]; then return; fi
    fi
    if ! kill -0 "$monitor_pid" 2>/dev/null || ((SECONDS >= deadline)); then
      echo "Monitor did not record $expected" >&2
      cat "$temp_dir/monitor.err" "$temp_dir/history.err" >&2
      exit 1
    fi
    sleep 0.2
  done
}

start_monitor
wait_for_health healthy
printf 'PASS: another process reads healthy history while monitoring\n'
rm -- "$AEGIS_DEMO_CONTROL_DIR/healthy"
wait_for_health unhealthy
grep -Fxq 'Health: healthy' <<<"$history"
printf 'PASS: history preserves healthy and unhealthy observations\n'
stop_monitor
history=$(read_history)
saved_count=$(grep -c '^Observed at: ' <<<"$history")
printf 'PASS: SIGTERM exits successfully and history survives shutdown\n'
touch "$AEGIS_DEMO_CONTROL_DIR/healthy"
start_monitor
wait_for_health healthy
[[ $(grep -c '^Observed at: ' <<<"$history") -gt $saved_count ]]
grep -Fxq 'Health: unhealthy' <<<"$history"
stop_monitor
printf 'PASS: restart reacquires the lock and appends to persisted history\n'
