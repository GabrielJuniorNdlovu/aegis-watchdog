# Run the monitoring demo

Run these commands from the repository root. You need Go 1.27, GCC with CGO
enabled, Docker, and Docker Compose. Docker Desktop must share the project
directory for the demo bind mount.

## Start the sandbox and monitor

In your first terminal:

```sh
CGO_ENABLED=1 go build -o bin/aegis ./cmd/aegis
export DOCKER_HOST="$(docker context inspect --format '{{.Endpoints.docker.Host}}')"
export AEGIS_DEMO_CONTROL_DIR="$PWD/data/monitor-demo-control"
mkdir -p "$AEGIS_DEMO_CONTROL_DIR"
chmod 755 "$AEGIS_DEMO_CONTROL_DIR"
touch "$AEGIS_DEMO_CONTROL_DIR/healthy"
docker compose -f demo/compose.yaml -f demo/monitor.compose.yaml -p aegis-monitor-demo up -d healthy
container_id="$(docker compose -f demo/compose.yaml -p aegis-monitor-demo ps -q healthy)"
./bin/aegis monitor --container "$container_id" --interval 200ms --failure-threshold 3 --recovery-threshold 2 --retention 168h --db data/monitor-demo/history.db
```

The monitor saves immediately. Docker may initially report `starting`; after a
few seconds, you should see `healthy`. Assessment initially reports `unknown`
until a check starts after the monitor startup boundary. Each result has a UTC time.

## Read history and change the demo health

In a second terminal at the repository root:

```sh
container_id="$(docker compose -f demo/compose.yaml -p aegis-monitor-demo ps -q healthy)"
DOCKER_HOST=invalid-Docker-host ./bin/aegis assessments --container "$container_id" --limit 20 --db data/monitor-demo/history.db
rm -- data/monitor-demo-control/healthy
```

The ID lookup uses the Docker CLI. The assessments command uses only SQLite, even
with invalid Docker settings. The sandbox health check tests whether its marker
exists; removing the marker changes only this demo's check result.

After a few seconds, read history again:

```sh
./bin/aegis assessments --container "$container_id" --limit 50 --db data/monitor-demo/history.db
./bin/aegis history --container "$container_id" --limit 20 --db data/monitor-demo/history.db
./bin/aegis errors --container "$container_id" --limit 20 --db data/monitor-demo/history.db
```

Assessment moves through `suspect` and reaches `persistent failure` after
three distinct failed checks. With one-second demo checks, this takes a few
seconds. Querying the last 50 rows retains the intermediate states even if you
miss them on screen. Docker aggregate health can become `unhealthy` earlier.
Repeated polls do not advance the failure count. Unhealthy applications are
successful collections, so the errors
query normally reports no collection errors in this scenario.

Restore the marker:

```sh
touch data/monitor-demo-control/healthy
```

Read assessments again. Two distinct successful checks produce `recovering`,
then `healthy`. Aegis does not restart the container. You can check the sandbox's
restart count and execution identity:

```sh
docker inspect --format '{{.RestartCount}} {{.State.StartedAt}}' "$container_id"
```

The restart count remains zero and execution start stays unchanged. Every saved
assessment says that no recovery action is authorized or executed.

## Stop and restart Aegis

In the first terminal, press Ctrl+C, then check its exit code and restart:

```sh
echo "$?"
./bin/aegis monitor --container "$container_id" --interval 200ms --failure-threshold 3 --recovery-threshold 2 --retention 168h --db data/monitor-demo/history.db
```

Normal shutdown returns `0`. Read history in the second terminal again. Existing
records remain, and the restarted monitor appends new observations and assessments.
The new process begins with empty streaks and waits for a newly started check.

Repeated rows can share one Docker health-check timestamp. They are repeated
observations of that check, not independent checks or a failure streak.

## Clean up the sandbox

Stop Aegis with Ctrl+C before removing its sandbox:

```sh
docker compose -f demo/compose.yaml -p aegis-monitor-demo down --timeout 5 --remove-orphans
```

The database remains in `data/monitor-demo/history.db`, so its history is still
readable after container removal. The lock file can remain without holding a
lock. No unrelated containers are removed, and no prune command is used.

To run the same scenario automatically with complete cleanup of its private
files, use `bash scripts/smoke-monitor.sh`.

## Run the controlled-recovery smoke test

This separate test creates only containers in a unique Compose project and uses
a private database under `data/`. It exercises a brief fault, one restart followed
by sustained checks, a persistent external fault that consumes the budget, record
mode against that same fault, and controller interruption during verification.
The test removes only that project's containers and temporary directory on exit.

```sh
bash scripts/smoke-recovery.sh
```

The script explicitly enables recovery only for its demo app with local-liveness
and restart-appropriate declarations. The Compose workloads use restart policy
`no`, no network, and bounded CPU, memory, and process counts. It never targets
the monitor demo database or unrelated containers. If stopped unexpectedly,
remove only the exact Compose project created by the script; do not use Docker
prune commands.

For a manual recovery run, build Aegis, create the external persistent-fault
control, and start only the self-healing workload:

```sh
CGO_ENABLED=1 go build -o bin/aegis ./cmd/aegis
export DOCKER_HOST="$(docker context inspect --format '{{.Endpoints.docker.Host}}')"
export AEGIS_RECOVERY_CONTROL_DIR="$PWD/data/recovery-demo-control"
mkdir -p data/recovery-demo
mkdir -p "$AEGIS_RECOVERY_CONTROL_DIR"
chmod 755 "$AEGIS_RECOVERY_CONTROL_DIR"
docker compose -f demo/recovery.compose.yaml -p aegis-recovery-demo up -d self-healing
container_id="$(docker compose -f demo/recovery.compose.yaml -p aegis-recovery-demo ps -q self-healing)"
./bin/aegis monitor --container "$container_id" --interval 200ms --db data/recovery-demo/history.db \
  --enable-recovery --service demo-self-healing \
  --healthcheck-local-liveness --restart-appropriate
```

After Aegis reports a persistent failure, it should reserve one attempt, capture
evidence, restart this exact demo container, and verify two distinct successful
checks. In a second terminal, read the action and confirm the new execution start time:

```sh
./bin/aegis actions --id 1 --db data/recovery-demo/history.db
docker inspect --format '{{.State.StartedAt}}' "$container_id"
```

To try record-only mode on the persistent fixture, create the marker first and
omit all recovery flags:

```sh
touch "$AEGIS_RECOVERY_CONTROL_DIR/fault"
docker compose -f demo/recovery.compose.yaml -p aegis-recovery-demo up -d persistent
persistent_id="$(docker compose -f demo/recovery.compose.yaml -p aegis-recovery-demo ps -q persistent)"
./bin/aegis monitor --container "$persistent_id" --interval 200ms --db data/recovery-demo/record.db
```

Stop Aegis with Ctrl+C before cleanup. Remove only the demo project and its
private data directory:

```sh
docker compose -f demo/recovery.compose.yaml -p aegis-recovery-demo down --timeout 5 --remove-orphans
rm -f -- data/recovery-demo/history.db data/recovery-demo/record.db data/recovery-demo-control/fault
rmdir -- data/recovery-demo data/recovery-demo-control
```
