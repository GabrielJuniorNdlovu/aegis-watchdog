#!/usr/bin/env bash
set -euo pipefail

if (($#)); then echo "Usage: bash scripts/smoke-recovery.sh" >&2; exit 2; fi
cd "$(dirname "$0")/.."
command -v go >/dev/null
docker compose version >/dev/null
if [[ -z ${DOCKER_HOST:-} ]]; then
  export DOCKER_HOST
  DOCKER_HOST=$(docker context inspect --format '{{.Endpoints.docker.Host}}')
fi
unset DOCKER_CONTEXT
docker version --format '{{.Server.Version}}' >/dev/null

mkdir -p data
temp_dir=$(mktemp -d "$PWD/data/aegis-recovery-demo.XXXXXXXX")
project=$(basename "$temp_dir" | tr '[:upper:].' '[:lower:]-')
export AEGIS_RECOVERY_CONTROL_DIR="$temp_dir/control"
mkdir "$AEGIS_RECOVERY_CONTROL_DIR"
chmod 755 "$AEGIS_RECOVERY_CONTROL_DIR"
touch "$AEGIS_RECOVERY_CONTROL_DIR/fault"
compose=(docker compose -f demo/recovery.compose.yaml -p "$project")
monitor_pid=""
cleanup() {
  local status=$?
  trap - EXIT
  if [[ -n $monitor_pid ]]; then kill -TERM "$monitor_pid" 2>/dev/null || true; wait "$monitor_pid" 2>/dev/null || true; fi
  "${compose[@]}" down --timeout 5 --remove-orphans >&2 || status=1
  if ((status == 0)); then
    rm -rf -- "$temp_dir"
  else
    echo "Smoke-test files preserved for diagnosis: $temp_dir" >&2
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
CGO_ENABLED=1 go build -o "$temp_dir/aegis" ./cmd/aegis

start_monitor() {
  local selector=$1 db=$2 recovery_args=${3:-}
  read -r -a extra <<<"$recovery_args"
  "$temp_dir/aegis" monitor --container "$selector" --interval 200ms --failure-threshold 3 --recovery-threshold 2 --retention 168h --db "$db" "${extra[@]}" >"$db.out" 2>"$db.err" &
  monitor_pid=$!
}
stop_monitor() { local p=$monitor_pid; monitor_pid=""; kill -TERM "$p" 2>/dev/null || true; wait "$p" || true; }
wait_text() {
  local file=$1 text=$2 error_file
  error_file=${file%.out}.err
  local timeout=${3:-50}
  local until=$((SECONDS + timeout))
  while ((SECONDS < until)); do
    grep -Fq "$text" "$file" 2>/dev/null && return 0
    kill -0 "$monitor_pid" 2>/dev/null || break
    sleep 0.2
  done
  cat "$file" "$error_file" >&2 2>/dev/null || true
  echo "Timed out waiting for: $text" >&2
  return 1
}
wait_action_state() {
  local db=$1 expected=$2 action action_id max_id=${4:-1}
  local timeout=${3:-75}
  local until=$((SECONDS + timeout))
  while ((SECONDS < until)); do
    for action_id in $(seq 1 "$max_id"); do
      if action=$("$temp_dir/aegis" actions --id "$action_id" --db "$db" 2>/dev/null) && grep -Fq "Current state: $expected" <<<"$action"; then echo "$action_id"; return 0; fi
    done
    kill -0 "$monitor_pid" 2>/dev/null || break
    sleep 0.2
  done
  echo "Action did not reach $expected" >&2
  for action_id in $(seq 1 "$max_id"); do "$temp_dir/aegis" actions --id "$action_id" --db "$db" >&2 || true; done
  cat "$db.out" "$db.err" >&2 2>/dev/null || true
  return 1
}
recovery_flags='--enable-recovery --service demo-app --healthcheck-local-liveness --restart-appropriate --recovery-startup-grace 0s --restart-stop-grace 2s --restart-request-timeout 30s --verification-deadline 20s --recovery-max-attempts 3 --recovery-window 2m --recovery-cooldown 1s'

# A brief fault must not cross the three-distinct-check threshold.
"${compose[@]}" up -d transient
transient_id=$("${compose[@]}" ps -q transient)
transient_start=$(docker inspect --format '{{.State.StartedAt}}' "$transient_id")
start_monitor "$transient_id" "$temp_dir/transient.db" "$recovery_flags"
sleep 16
stop_monitor
[[ $(docker inspect --format '{{.State.StartedAt}}' "$transient_id") == "$transient_start" ]]
echo "PASS: transient health fault did not trigger a restart"

# A container-local fault resets on restart; distinct fresh checks prove recovery.
"${compose[@]}" up -d self-healing
self_id=$("${compose[@]}" ps -q self-healing)
self_start=$(docker inspect --format '{{.State.StartedAt}}' "$self_id")
start_monitor "$self_id" "$temp_dir/self-healing.db" "$recovery_flags"
success_action=$(wait_action_state "$temp_dir/self-healing.db" succeeded 75 3)
[[ $(docker inspect --format '{{.State.StartedAt}}' "$self_id") != "$self_start" ]]
echo "PASS: action $success_action restarted once and passed sustained healthy checks"
stop_monitor

# The external marker survives restarts; three attempts are consumed and blocked.
"${compose[@]}" up -d persistent
persistent_id=$("${compose[@]}" ps -q persistent)
persistent_start=$(docker inspect --format '{{.State.StartedAt}}' "$persistent_id")
flags=${recovery_flags/--verification-deadline 20s/--verification-deadline 3s}
start_monitor "$persistent_id" "$temp_dir/persistent.db" "$flags"
wait_text "$temp_dir/persistent.db.out" 'budget_exhausted' 100
[[ $(docker inspect --format '{{.State.StartedAt}}' "$persistent_id") != "$persistent_start" ]]
budget_output=$("$temp_dir/aegis" budget --service demo-app --db "$temp_dir/persistent.db")
grep -Fq 'Attempts within' <<<"$budget_output"
grep -Eq 'Attempts within .*: 3$' <<<"$budget_output"
dispatches=0
for action_id in 1 2 3; do
  action=$("$temp_dir/aegis" actions --id "$action_id" --db "$temp_dir/persistent.db")
  if grep -Fq 'Current state: succeeded' <<<"$action"; then echo "Persistent fault was reported recovered" >&2; exit 1; fi
  if grep -Fq 'Transition: planned -> dispatching' <<<"$action"; then
    ((dispatches += 1))
    grep -Fq 'Current state: failed' <<<"$action"
  else
    grep -Fq 'Current state: canceled' <<<"$action"
    grep -Fq 'evidence_failed' <<<"$action"
  fi
done
((dispatches >= 2))
echo "PASS: persistent fault survived $dispatches confirmed restarts; the three-attempt budget exhausted without false success"
stop_monitor

# Record mode observes the same fault without mutating the container.
before=$(docker inspect --format '{{.State.StartedAt}}' "$persistent_id")
start_monitor "$persistent_id" "$temp_dir/record.db"
sleep 4
stop_monitor
[[ $(docker inspect --format '{{.State.StartedAt}}' "$persistent_id") == "$before" ]]
echo "PASS: record mode did not restart the persistently unhealthy container"

# Interrupt verification, then let startup reconciliation preserve uncertainty.
interrupted_start=$(docker inspect --format '{{.State.StartedAt}}' "$self_id")
interrupted_flags=${recovery_flags/--service demo-app/--service demo-interrupt}
interrupted_flags=${interrupted_flags/--recovery-max-attempts 3/--recovery-max-attempts 10}
start_monitor "$self_id" "$temp_dir/interrupted.db" "$interrupted_flags"
wait_text "$temp_dir/interrupted.db.out" 'restart accepted; verifying' 75
stop_monitor
start_monitor "$self_id" "$temp_dir/interrupted.db"
sleep 2
stop_monitor
interrupted_action=""
for action_id in $(seq 1 10); do
  action=$("$temp_dir/aegis" actions --id "$action_id" --db "$temp_dir/interrupted.db" 2>/dev/null || true)
  if grep -Fq 'Current state: uncertain' <<<"$action" && grep -Fq 'Transition: planned -> dispatching' <<<"$action"; then interrupted_action=$action_id; break; fi
done
[[ -n $interrupted_action ]]
action=$("$temp_dir/aegis" actions --id "$interrupted_action" --db "$temp_dir/interrupted.db")
grep -Fq 'Current state: uncertain' <<<"$action"
[[ $(grep -Fc 'Transition: planned -> dispatching' <<<"$action") == 1 ]]
[[ $(docker inspect --format '{{.State.StartedAt}}' "$self_id") != "$interrupted_start" ]]
echo "PASS: interrupted verification reconciled to uncertain without a blind retry"
