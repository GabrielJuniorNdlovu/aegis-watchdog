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

wait_health() {
  local id=$1 expected=$2 timeout=${3:-30} until
  until=$((SECONDS + timeout))
  while ((SECONDS < until)); do
    [[ $(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$id" 2>/dev/null || true) == "$expected" ]] && return 0
    sleep 0.2
  done
  echo "Timed out waiting for $id health $expected" >&2
  return 1
}
write_manifest() {
	local path=$1 service=$2 target=$3 dependency_container=$4
  printf '{"services":[{"id":"%s","container":"%s","dependencies":["database"]},{"id":"database","container":"%s"}]}\n' \
    "$service" "$target" "$dependency_container" >"$path"
}

start_monitor() {
  local selector=$1 db=$2 recovery_args=${3:-}
  # Evidence names belong to one journal, so scenarios must not share a directory.
  mkdir -p "$(dirname "$db")"
  read -r -a extra <<<"$recovery_args"
  "$temp_dir/aegis" monitor --container "$selector" --interval 200ms --failure-threshold 3 --recovery-threshold 2 --retention 168h --db "$db" "${extra[@]}" >"$db.out" 2>"$db.err" &
  monitor_pid=$!
}
stop_monitor() { local p=$monitor_pid; monitor_pid=""; kill -TERM "$p"; wait "$p"; }
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

# This healthy dependency uses an independent local-liveness check.
"${compose[@]}" up -d database
database_id=$("${compose[@]}" ps -q database)
wait_health "$database_id" healthy

# A brief fault must not cross the three-distinct-check threshold.
"${compose[@]}" up -d transient
transient_id=$("${compose[@]}" ps -q transient)
transient_manifest="$temp_dir/transient-services.json"
write_manifest "$transient_manifest" demo-transient "$transient_id" "$database_id"
transient_start=$(docker inspect --format '{{.State.StartedAt}}' "$transient_id")
transient_flags=${recovery_flags/--service demo-app/--service demo-transient}
transient_flags+=" --service-config $transient_manifest"
start_monitor "$transient_id" "$temp_dir/transient/history.db" "$transient_flags"
sleep 16
stop_monitor
[[ $(docker inspect --format '{{.State.StartedAt}}' "$transient_id") == "$transient_start" ]]
echo "PASS: transient health fault did not trigger a restart"

# A container-local fault resets on restart; distinct fresh checks prove recovery.
"${compose[@]}" up -d self-healing
self_id=$("${compose[@]}" ps -q self-healing)
healthy_manifest="$temp_dir/healthy-services.json"
write_manifest "$healthy_manifest" demo-app "$self_id" "$database_id"
healthy_flags="$recovery_flags --service-config $healthy_manifest"
self_start=$(docker inspect --format '{{.State.StartedAt}}' "$self_id")
start_monitor "$self_id" "$temp_dir/self-healing/history.db" "$healthy_flags"
wait_text "$temp_dir/self-healing/history.db.out" 'restart accepted; verifying' 75
touch "$AEGIS_RECOVERY_CONTROL_DIR/dependency-fault"
wait_health "$database_id" unhealthy
success_action=$(wait_action_state "$temp_dir/self-healing/history.db" succeeded 75 3)
success_history=$("$temp_dir/aegis" actions --id "$success_action" --db "$temp_dir/self-healing/history.db")
grep -Fq 'Current state: succeeded' <<<"$success_history"
grep -Eq '^Evidence status: (complete|partial)$' <<<"$success_history"
grep -Fq 'Published bundle: ' <<<"$success_history"
[[ -s "$temp_dir/self-healing/evidence/capture-$success_action.json" ]]
[[ $(docker inspect --format '{{.State.StartedAt}}' "$self_id") != "$self_start" ]]
echo "PASS: action $success_action history and published evidence confirm sustained recovery"
stop_monitor
rm -f -- "$AEGIS_RECOVERY_CONTROL_DIR/dependency-fault"
wait_health "$database_id" healthy

# An unhealthy direct dependency blocks before reserving budget.
wait_health "$self_id" unhealthy
touch "$AEGIS_RECOVERY_CONTROL_DIR/dependency-fault"
wait_health "$database_id" unhealthy
blocked_manifest="$temp_dir/blocked-services.json"
write_manifest "$blocked_manifest" demo-app "$self_id" "$database_id"
blocked_flags="$recovery_flags --service-config $blocked_manifest"
blocked_start=$(docker inspect --format '{{.State.StartedAt}}' "$self_id")
start_monitor "$self_id" "$temp_dir/dependency-blocked/history.db" "$blocked_flags"
wait_text "$temp_dir/dependency-blocked/history.db.out" 'dependency gate blocked' 50
stop_monitor
blocked_budget=$("$temp_dir/aegis" budget --service demo-app --db "$temp_dir/dependency-blocked/history.db")
grep -Eq 'Attempts within .*: 0$' <<<"$blocked_budget"
[[ $(docker inspect --format '{{.State.StartedAt}}' "$self_id") == "$blocked_start" ]]
echo "PASS: unhealthy dependency blocked recovery before reservation; budget remained zero"

# A missing Docker health check is unknown and also blocks without a reservation.
"${compose[@]}" up -d dependency-no-health
no_health_id=$("${compose[@]}" ps -q dependency-no-health)
no_health_manifest="$temp_dir/no-health-services.json"
write_manifest "$no_health_manifest" demo-app "$self_id" "$no_health_id"
no_health_flags="$recovery_flags --service-config $no_health_manifest"
start_monitor "$self_id" "$temp_dir/dependency-missing-health/history.db" "$no_health_flags"
wait_text "$temp_dir/dependency-missing-health/history.db.out" 'health_check_missing' 50
stop_monitor
no_health_budget=$("$temp_dir/aegis" budget --service demo-app --db "$temp_dir/dependency-missing-health/history.db")
grep -Eq 'Attempts within .*: 0$' <<<"$no_health_budget"
echo "PASS: dependency without health evidence blocked recovery without consuming budget"

# Dependency recovery requires a new, fresh monitor evaluation; no old action was queued.
rm -f -- "$AEGIS_RECOVERY_CONTROL_DIR/dependency-fault"
wait_health "$database_id" healthy
recovered_manifest="$temp_dir/recovered-services.json"
write_manifest "$recovered_manifest" demo-app "$self_id" "$database_id"
recovered_flags="$recovery_flags --service-config $recovered_manifest"
start_monitor "$self_id" "$temp_dir/dependency-recovered/history.db" "$recovered_flags"
recovered_action=$(wait_action_state "$temp_dir/dependency-recovered/history.db" succeeded 75 3)
recovered_action_history=$("$temp_dir/aegis" actions --id "$recovered_action" --db "$temp_dir/dependency-recovered/history.db")
grep -Fq 'dependency_gate_revalidated' <<<"$recovered_action_history"
echo "PASS: recovered dependency was freshly reevaluated before action $recovered_action"
stop_monitor

# The external marker survives restarts; three attempts are consumed and blocked.
"${compose[@]}" up -d persistent
persistent_id=$("${compose[@]}" ps -q persistent)
persistent_start=$(docker inspect --format '{{.State.StartedAt}}' "$persistent_id")
flags=${recovery_flags/--verification-deadline 20s/--verification-deadline 3s}
start_monitor "$persistent_id" "$temp_dir/persistent/history.db" "$flags"
wait_text "$temp_dir/persistent/history.db.out" 'budget_exhausted' 100
[[ $(docker inspect --format '{{.State.StartedAt}}' "$persistent_id") != "$persistent_start" ]]
budget_output=$("$temp_dir/aegis" budget --service demo-app --db "$temp_dir/persistent/history.db")
grep -Fq 'Attempts within' <<<"$budget_output"
grep -Eq 'Attempts within .*: 3$' <<<"$budget_output"
dispatches=0
for action_id in 1 2 3; do
  action=$("$temp_dir/aegis" actions --id "$action_id" --db "$temp_dir/persistent/history.db")
  if grep -Fq 'Current state: succeeded' <<<"$action"; then echo "Persistent fault was reported recovered" >&2; exit 1; fi
  if ! grep -Fq 'Transition: planned -> dispatching' <<<"$action" || ! grep -Fq 'Current state: failed' <<<"$action"; then
    printf '%s\n' "$action" >&2
    echo 'Every persistent-fault attempt must dispatch and fail verification.' >&2
    exit 1
  fi
  grep -Fq "Verification target: $persistent_id" <<<"$action"
  grep -Fq 'Outcome reason: verification_deadline_unhealthy' <<<"$action"
  ((dispatches += 1))
done
((dispatches == 3))
echo "PASS: persistent fault survived $dispatches confirmed restarts; the three-attempt budget exhausted without false success"
stop_monitor

# Record mode observes the same fault without mutating the container.
before=$(docker inspect --format '{{.State.StartedAt}}' "$persistent_id")
start_monitor "$persistent_id" "$temp_dir/record/history.db"
sleep 4
stop_monitor
[[ $(docker inspect --format '{{.State.StartedAt}}' "$persistent_id") == "$before" ]]
echo "PASS: record mode did not restart the persistently unhealthy container"

# Interrupt verification, then let startup reconciliation preserve uncertainty.
interrupted_start=$(docker inspect --format '{{.State.StartedAt}}' "$self_id")
interrupted_flags=${recovery_flags/--service demo-app/--service demo-interrupt}
interrupted_flags=${interrupted_flags/--recovery-max-attempts 3/--recovery-max-attempts 10}
start_monitor "$self_id" "$temp_dir/interrupted/history.db" "$interrupted_flags"
wait_text "$temp_dir/interrupted/history.db.out" 'restart accepted; verifying' 75
interrupted_execution=$(docker inspect --format '{{.State.StartedAt}}' "$self_id")
stop_monitor
start_monitor "$self_id" "$temp_dir/interrupted/history.db"
sleep 2
stop_monitor
interrupted_action=""
for action_id in $(seq 1 10); do
  action=$("$temp_dir/aegis" actions --id "$action_id" --db "$temp_dir/interrupted/history.db" 2>/dev/null || true)
  if grep -Fq 'Current state: uncertain' <<<"$action" && grep -Fq 'Transition: planned -> dispatching' <<<"$action"; then interrupted_action=$action_id; break; fi
done
[[ -n $interrupted_action ]]
action=$("$temp_dir/aegis" actions --id "$interrupted_action" --db "$temp_dir/interrupted/history.db")
grep -Fq 'Current state: uncertain' <<<"$action"
[[ $(grep -Fc 'Transition: planned -> dispatching' <<<"$action") == 1 ]]
[[ $(docker inspect --format '{{.State.StartedAt}}' "$self_id") != "$interrupted_start" ]]
[[ $(docker inspect --format '{{.State.StartedAt}}' "$self_id") == "$interrupted_execution" ]]
interrupted_budget=$("$temp_dir/aegis" budget --service demo-interrupt --db "$temp_dir/interrupted/history.db")
grep -Eq 'Attempts within .*: 1$' <<<"$interrupted_budget"
echo "PASS: interrupted verification reconciled to uncertain without a blind retry"
