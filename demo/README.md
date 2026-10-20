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
./bin/aegis monitor --container "$container_id" --interval 1s --retention 168h --db data/monitor-demo/history.db
```

The monitor saves immediately. Docker may initially report `starting`; after a
few seconds, you should see `healthy`. Each displayed observation has a UTC time.

## Read history and change the demo health

In a second terminal at the repository root:

```sh
container_id="$(docker compose -f demo/compose.yaml -p aegis-monitor-demo ps -q healthy)"
DOCKER_HOST=invalid-Docker-host ./bin/aegis history --container "$container_id" --limit 20 --db data/monitor-demo/history.db
rm -- data/monitor-demo-control/healthy
```

The ID lookup uses the Docker CLI. The history command uses only SQLite, even
with invalid Docker settings. The sandbox health check tests whether its marker
exists; removing the marker changes only this demo's check result.

After a few seconds, read history again:

```sh
./bin/aegis history --container "$container_id" --limit 20 --db data/monitor-demo/history.db
./bin/aegis errors --container "$container_id" --limit 20 --db data/monitor-demo/history.db
```

You should see recent `unhealthy` observations followed by older `healthy`
observations. Unhealthy applications are successful collections, so the errors
query normally reports no collection errors in this scenario.

Restore the marker:

```sh
touch data/monitor-demo-control/healthy
```

## Stop and restart Aegis

In the first terminal, press Ctrl+C, then check its exit code and restart:

```sh
echo "$?"
./bin/aegis monitor --container "$container_id" --interval 1s --retention 168h --db data/monitor-demo/history.db
```

Normal shutdown returns `0`. Read history in the second terminal again. Existing
records remain, and the restarted monitor appends new observations.

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
