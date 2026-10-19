# Aegis

Aegis currently inspects one explicitly selected Docker container. It reports
Docker lifecycle and application health separately and takes no recovery action.
The [project contract](AEGIS_PROJECT_GOAL.md) describes later work, not implemented features.

## Build and inspect a container

Install Go 1.27 or newer and use a local Linux Docker Engine. Build the executable:

```sh
go build -o bin/aegis ./cmd/aegis
```

Supply one container name, full ID, or unambiguous ID prefix:

```sh
./bin/aegis inspect --container my-app
./bin/aegis inspect --container 0123456789ab
./bin/aegis inspect --help
```

If Docker uses a CLI context, such as Docker Desktop, select its endpoint first:

```sh
export DOCKER_HOST="$(docker context inspect --format '{{.Endpoints.docker.Host}}')"
```

Aegis uses the Docker SDK's `DOCKER_HOST`, `DOCKER_TLS_VERIFY`, `DOCKER_CERT_PATH`,
and `DOCKER_API_VERSION` settings. It does not load Docker CLI contexts itself.
Without `DOCKER_HOST`, the SDK uses its default socket, `/var/run/docker.sock` on Linux.
Leave `DOCKER_API_VERSION` unset for automatic negotiation. The pinned SDK supports
API versions 1.40 through 1.56 and negotiates down to the daemon's supported version.

Example output:

```text
Container: my-app
Docker ID: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
Lifecycle: running
Health: healthy
Last completed health check: 2026-10-19T10:00:00Z (age: 30s)
Explanation: Docker reports that the application's health check is passing.
Action: none — inspection only.
```

Successful output goes to stdout. Operational errors and argument errors go to stderr.
Exit codes are:

- `0`: inspection succeeded, including `unhealthy`, `starting`, and `unknown` health. Help also exits `0`.
- `1`: inspection, Docker connection configuration, or output failed.
- `2`: invalid command or arguments, including a missing, empty, or repeated container selector.

Use the reported health to assess the application. The exit code reports whether
inspection worked. A missing container or failed Docker request is an unavailable
observation, not an unhealthy application.

## Run automated checks

These checks need no Docker daemon:

```sh
test -z "$(gofmt -l cmd internal)"
go test ./...
go vet ./...
go build -o bin/aegis ./cmd/aegis
```

Tests run the real Docker SDK against a local fake HTTP API, including negotiation,
error responses, incomplete payloads, timeouts, and cancellation. They assert the
expected read-only requests and check that secret fixture values stay out of output.

## Run the isolated demo

Install Docker Compose v2 or newer and start the Docker daemon. The demo pulls
`busybox:1.37.0` and runs three containers with bounded CPU, memory, and process limits.
It exposes no ports and disables restart policies.

Run the repeatable smoke test:

```sh
bash scripts/smoke-inspect.sh
```

The script builds a temporary binary, creates a unique Compose project, waits up
to 60 seconds for the expected health states, and verifies exit `0` for all three.
Its exit trap removes only that project's containers and temporary files.
The shared BusyBox image remains cached. No prune command is used.

To inspect the demo manually:

```sh
docker compose -f demo/compose.yaml -p aegis-inspection-demo up -d
export DOCKER_HOST="$(docker context inspect --format '{{.Endpoints.docker.Host}}')"
./bin/aegis inspect --container "$(docker compose -f demo/compose.yaml -p aegis-inspection-demo ps -q healthy)"
./bin/aegis inspect --container "$(docker compose -f demo/compose.yaml -p aegis-inspection-demo ps -q unhealthy)"
./bin/aegis inspect --container "$(docker compose -f demo/compose.yaml -p aegis-inspection-demo ps -q no-healthcheck)"
```

The health checks may initially report `starting`. Repeat inspection after a few
seconds to see `healthy` and `unhealthy`. The third container reports `unknown`.

Remove the manual demo:

```sh
docker compose -f demo/compose.yaml -p aegis-inspection-demo down --timeout 5 --remove-orphans
```

## Inspection semantics and limits

The CLI makes one container inspection request, plus the SDK's read-only API
negotiation requests. It never lists containers, retrieves application logs, or
creates, starts, stops, restarts, or deletes containers. Demo setup and cleanup
use Docker Compose to manage only demo resources.

A five-second deadline covers negotiation and inspection together. SIGINT and
SIGTERM cancel pending requests. Aegis adds no retries. SDK transport behavior
and supported API versions follow the pinned dependency.

Lifecycle describes Docker's process state. Health describes Docker's configured
application check, whose meaning depends on that application's configuration.
Missing or disabled checks mean `unknown`. A configured check without a Docker
health status also means `unknown`. Missing check history leaves the timestamp
unavailable without changing a valid health status.

The latest completed check's end timestamp supplies its age, calculated once at
output time. Future timestamps produce an unknown age and a clock explanation.
Docker retains only recent check history. A stopped or paused container can retain
health data from earlier execution. Inspection shows that data separately from
lifecycle and explains that it does not establish current availability.

Inspection is a snapshot. State can change immediately afterward. Aegis displays
age but applies no freshness threshold and runs no independent application probe.
It stores no observations, runs no monitoring loop, and implements no recovery.

Aegis displays only allowlisted metadata and its own explanations. It does not
print environment variables, health-check commands, check output, full inspection
JSON, or raw Docker error bodies. Docker sends the full inspection response to the
SDK in memory; this is not a filtered Docker endpoint. Container names and IDs
appear in output, so do not put secrets in names.

Docker socket access is privileged. Read-only inspection is a restriction in this
program's behavior, not a security sandbox or a restriction on socket permissions.
