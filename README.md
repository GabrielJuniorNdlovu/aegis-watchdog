# Aegis

Aegis inspects one explicitly selected Docker container and can save the result to
SQLite. It reports lifecycle and application health separately, reads saved
history without Docker, and monitors one target periodically with deterministic
health assessments and persisted explanations. It takes no recovery action.
The [project contract](AEGIS_PROJECT_GOAL.md) describes later work, not implemented features.

## Build and inspect a container

Install Go 1.27 or newer and a C compiler, such as GCC. Use a local Linux Docker
Engine for inspection. The SQLite driver requires CGO at build time:

```sh
CGO_ENABLED=1 go build -o bin/aegis ./cmd/aegis
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

- `0`: inspection and any requested saving succeeded. Unhealthy or unknown health does not change this. Read-only queries, including empty results, help, and normal monitor shutdown also exit `0`.
- `1`: inspection, persistence, history access, connection configuration, monitor locking, or output failed.
- `2`: invalid command or arguments, including an invalid selector, path, limit, interval, or retention duration.

Use the reported health to assess the application. The exit code reports whether
inspection worked. A missing container or failed Docker request is an unavailable
observation, not an unhealthy application.

## Save an observation and read history

Add `--save` to persist the successful inspection. Aegis uses the existing result
and does not inspect Docker a second time:

```sh
./bin/aegis inspect --container my-app --save
```

The default database is `./data/aegis.db`, relative to your current working
directory. Override the file path with `--db` on either command:

```sh
./bin/aegis inspect --container my-app --save --db data/my-observations.db
./bin/aegis history --container FULL_DOCKER_ID --limit 20 --db data/my-observations.db
```

Replace `FULL_DOCKER_ID` with the full 64-character lowercase ID printed by
inspection. History accepts no names or ID prefixes and never connects to Docker.
Container replacements get separate histories, even when their names match.

History displays the observation time in UTC and labels the results as historical.
It returns newest observations first. For equal observation times, the most
recently inserted row appears first. The default limit is 20; valid limits are
integers from 1 through 1000. Empty history succeeds with a clear message.

Without `--save`, inspection never creates or opens a database; `--db` is ignored.
Failed Docker inspections save nothing. If inspection succeeds but saving fails,
Aegis reports that distinction on stderr and exits `1`. If output fails after a
committed save, the error states that the observation was saved.

Each observation stores the full Docker ID, observed name, UTC collection time,
lifecycle, health, and nullable completed health-check time. Schema 3 also stores
execution start time, whether a check is configured, and individual check start,
end, and nullable exit code. An internal sequence number breaks ordering ties.
Observations exclude free-form inspection explanations, environment variables,
check commands, and log output. Assessments store Aegis-authored explanations.
Each explicit save inserts a row, even when the
health-check timestamp repeats. These rows do not represent independent checks
and are not failure streaks.

The initial schema migration and first insert commit together. Schema version 5
uses `PRAGMA user_version`, a DELETE rollback journal, `synchronous=FULL`, a
one-second busy timeout, and parameterized queries. Each database operation has
a five-second context deadline, separate from the Docker request deadline.
Writes require DELETE journal mode. SQLite serializes writers; a competing write
can fail with a lock error after the bounded wait.

History uses SQLite `mode=ro`, performs no migrations, and does not create a
missing database. Both commands reject newer schema versions without changing
their contents. Timestamps use fixed-width UTC text internally so sorting remains
correct across fractional seconds. Newly created database files have mode `0600`;
new parent directories have mode `0700`. Existing permissions are preserved.
Runtime databases and SQLite sidecar files are ignored by Git.

The pinned `github.com/mattn/go-sqlite3` driver bundles SQLite and uses Go's
standard `database/sql` API. You do not need a separate SQLite server or library.
Builds need CGO and a C compiler; cross-compilation needs a matching C toolchain.

Collection failures have a separate history, queried by the exact requested
target rather than a guessed Docker ID:

```sh
./bin/aegis errors --container my-app --limit 20 --db data/aegis.db
```

The errors command opens SQLite read-only and requires no Docker connection.
Versions 1 through 4 remain readable without migration; the next write upgrades
them to version 5 atomically. Observations written before schema 3 have no
individual-check evidence and cannot establish an assessment streak. Older Aegis
binaries reject the newer schema.

## Monitor one container

```sh
./bin/aegis monitor --container my-app --interval 5s --retention 168h --db data/aegis.db
```

Monitoring inspects immediately, saves the result, then waits the interval after
completion before trying again. Only one inspection runs at a time; missed time
does not accumulate. Docker collection failures go to the separate errors history
and monitoring continues at the same cadence. Any persistence failure stops it.
Ctrl+C and SIGTERM cancel outstanding work and exit successfully.

The default interval is five seconds. Both interval and retention accept positive
Go durations such as `200ms`, `5s`, or `168h`; `7d` is not a supported duration.
Each Docker request, including negotiation, has a five-second deadline. Each
SQLite operation has a separate five-second deadline and a one-second lock wait.
Shutdown cancels pending collection or storage and does not record user
cancellation as an application failure. Only committed records survive.

With a name selector, every cycle resolves the name again. If a replacement takes
that name, its actual Docker ID starts a separate successful-observation history.
With an ID selector, a removed container produces collection errors until that
same target is available again. Aegis never switches an ID selector to another ID.
Error history uses the exact requested selector, including any leading slash,
and can span name reuse. Errors carry no fabricated container ID or health status.
The categories are `not_found`, `unavailable`, `permission_denied`, `timeout`,
`invalid_response`, `canceled`, and `request_failed`. Explanations are Aegis-authored
and at most 256 characters; Docker error payloads are never persisted.

Only one monitor can use a data directory at a time, including different database
filenames in that directory. A Linux advisory file lock is acquired before
migrations or writes and released by the kernel when the process exits or crashes.
The `.aegis-monitor.lock` file remains; its existence does not mean a monitor is
running. Do not unlink it while a monitor is active. Read-only history and errors
commands remain available while monitoring runs.

Retention defaults to seven days (`168h`) and must be positive. After the first
collection, then at most once per minute after a completed cycle, the monitor
deletes at most 500 expired rows from each of observations, collection errors,
and assessments in one transaction. Expiration uses observation, collection, or
evaluation time, never health-check time. Each retained assessment carries its
policy, counts, last check summary, identity, and explanation independently of
raw observation retention. Rows exactly at the cutoff and future-dated rows
remain. A large backlog
can take multiple batches to clear; the duration is not a strict disk-size cap.
Retention applies across all container IDs and targets in the selected database.
No action, incident, or recovery-budget table is pruned. Deletion makes SQLite
pages reusable but may not shrink the file; monitoring never runs `VACUUM`.

## Assess health during monitoring

```sh
./bin/aegis monitor --container my-app --interval 5s --failure-threshold 3 --recovery-threshold 2 --max-evidence-age 1m --max-check-gap 1m --db data/aegis.db
./bin/aegis assessments --container my-app --limit 20 --db data/aegis.db
```

Run the second command in another terminal. Assessment history filters the exact
selector used by the monitor, including names, ID prefixes, or a leading slash.
It can span container replacements when a name is reused. Each successful row
shows the resolved full ID and execution start time. Collection-error rows have
no container ID. The command uses SQLite read-only, never connects to Docker,
and returns newest evaluation times first, with insertion order breaking ties.
It displays historical evaluation and collection times, evidence age at evaluation
and at read time, counts, effective policy, stable reason code, and explanation.
Empty history exits `0`; invalid arguments exit `2`; read failures exit `1`.

The default policy requires three failed checks for `persistent failure` and two
successful checks after persistent failure to establish `healthy`. All four
policy options must be positive. Individual check exit code zero succeeds; a
positive code fails. Missing or negative exit codes are unusable. Docker aggregate
health, such as `starting` or `unhealthy`, is recorded separately and does not
increment a streak. Aegis uses Docker's configured probe, with no separate probe.

Assessment transitions follow these rules:

- A usable success establishes `healthy` unless persistent failure is active.
- Failures below the failure threshold produce `suspect`. Reaching it produces
  `persistent failure`.
- Success after persistent failure produces `recovering` until the recovery
  threshold. A failure during recovery returns to `persistent failure`.
- Collection errors, missing checks, non-running lifecycle, stale evidence,
  unusable timestamps, or excessive gaps produce `unknown` and clear streaks.
  A prior persistent failure remains unverified. Fresh successes must still meet
  the recovery threshold.

Evidence and collection time may be at most 60 seconds old by default. Consecutive
observed check completions may be at most 60 seconds apart. Equality is usable;
exceeding either limit breaks continuity. These limits allow a 30-second check
interval plus execution time. Fresh duplicates neither increment nor
reset a streak. Newly observed checks are processed chronologically, using full
ID, execution start, and check timestamps for identity. Newly arriving older,
conflicting, overlapping, or future-dated evidence produces explained `unknown`.
Docker's bounded log can omit checks. Counts mean consecutive observed outcomes
within these continuity rules, not proof that every check was captured. Configure
age and gap to match the application's check interval and inspection cadence.

A new monitor starts with empty state and never restores streaks from SQLite.
It ignores checks that start before process startup or end at or before startup.
Replacement, restart, or a policy change also starts fresh and clears any prior
persistent-failure condition. Evidence gaps establish a new boundary too, while
preserving that condition. Checks must start at or after the boundary and finish
after it. This conservative rule waits for a newly started check, even if Docker
still reports a previous aggregate status. History remains available.

Each monitoring result and its assessment commit in one transaction. Persistence
failure stops monitoring. Every assessment states: "No recovery action is
authorized or executed." `recovering` describes successful application checks;
Aegis has taken no action to cause them. One-off `inspect --save` does not run the
evaluator. See the [evidence design note](docs/health-assessment-design.md).

## Inspect recovery budgets and reservations

Feature 5 adds durable reservation and budget logic. Reservation creation is an
internal operation exercised through temporary-database tests. Live monitoring
does not create reservations, and no Docker restart operation is connected.

Inspect the host journal without Docker:

```sh
./bin/aegis budget --service orders-api --db data/aegis.db
./bin/aegis budget --service orders-api --max-attempts 3 --budget-window 1h --cooldown 5m --limit 20 --db data/aegis.db
```

`orders-api` is an explicit logical service ID, not a Docker container name.
Existing monitor runs have not registered any logical services. For those
journals, this command reports no reservations and insufficient history coverage.
It does not create sample attempts or change the database. The policy flags
select the inspection policy; each reservation also displays its saved policy.

The default budget allows three attempts within `(now - 1h, now]` and ends
cooldown exactly five minutes after the last reservation. All committed
reservations count, including canceled and unsuccessful attempts. Any unresolved
reservation blocks all services in the host journal. Startup cancels an interrupted
planned reservation without refunding it. Dispatching and verifying actions become
uncertain and require explicit acknowledgment. Budget availability does not
authorize a restart.

The first accepted reservation records known coverage for its initial window.
Policy changes retain all attempts; a window extending before known coverage is
blocked until its left boundary reaches coverage. A durable clock high-water
mark blocks backward movement with zero tolerance until time catches up.
Reservation calls persist this timing evidence even when policy blocks them.
Read-only inspection does not update it.

Use one canonical journal per host. Routine retention never prunes reservations,
coverage, or clock metadata. Optional linked assessment snapshots survive
observation and assessment pruning. Action audit and acknowledgment records are
retained too. See the reconciliation procedure below.

Run the isolated reservation scenarios without Docker or an operational database:

```sh
go test -v ./internal/store -run 'TestReservation|TestCooldown|TestBudget|TestPolicyChange|TestConcurrent|TestFuture|TestEveryReservation|TestAssessmentReference|TestRetentionPreservesAll'
```

Each test uses private temporary SQLite files and removes them afterward. Tests
use terminal-state fixtures to check budget and cooldown independently of the
unresolved-action block. Fixtures seed states only in private temporary journals;
no public command manufactures attempts.
See [reservation semantics](docs/recovery-reservations.md) for exact boundaries,
clock behavior, service identity, concurrency, and retention limits.

## Reconcile interrupted actions and acknowledge uncertainty

Monitoring acquires its exclusive owner lock and reconciles unfinished actions
before collecting observations. A planned action becomes canceled because no
mutation was dispatched. An interrupted dispatch or verification inspects only
the recorded full Docker ID, then becomes uncertain. Startup never resends an
operation or infers success from one healthy result or a changed execution start.
If recording reconciliation fails, monitoring stops before collection. Once
uncertainty is recorded, read-only monitoring can continue; reservations remain
blocked. Existing uncertain and terminal outcomes are unchanged on later startup.

Find recorded action IDs in the budget command's reservation history. Inspect an
existing action without Docker:

```sh
read -r -p 'Existing action ID from budget history: ' action_id
./bin/aegis actions --id "$action_id" --db data/aegis.db
```

The output includes original intent, policy, exact target, current state, audit
history, recorded inspection evidence or its absence, acknowledgment, and any
remaining unresolved-action block. All inspection evidence is historical.

To acknowledge an uncertain outcome, first stop the monitor with Ctrl+C. Review
the action and its evidence, then provide your explanation:

```sh
read -r -p 'Reason for accepting this uncertainty: ' reason
./bin/aegis actions acknowledge --id "$action_id" --reason "$reason" --db data/aegis.db
./bin/aegis actions --id "$action_id" --db data/aegis.db
```

The reason must contain 1 to 512 UTF-8 bytes, include non-whitespace text, and
contain no control characters. Acknowledgment refuses while a daemon owns the
same data directory. It accepts only an uncertain action. The state stays
`uncertain`; acknowledgment time and reason resolve its block. Original intent,
uncertainty, inspection evidence, and consumed attempt remain. A repeat returns
`already acknowledged` and preserves the original time and reason. Budget,
cooldown, clock, and other prerequisites still apply. No new action is reserved
and no Docker operation is executed. Successful and repeated acknowledgment return
`0`; invalid arguments return `2`; unknown IDs, wrong states, lock conflicts, and
persistence failures return `1`.

Normal monitoring currently creates no reservations, so an operational journal
may contain no action IDs. Do not manufacture actions in it to try the command.
Run the repeatable simulated demonstration instead:

```sh
bash scripts/smoke-actions.sh
```

This uses test-only lifecycle fixtures, a fake Docker API, private temporary
SQLite files, and real CLI child processes. It demonstrates interrupted
verification becoming uncertain, monitoring continuing, acknowledgment refusing
while the daemon runs, explicit acknowledgment after shutdown, and preserved
audit and budget history. Cleanup removes only its temporary files and processes.
It needs Go, CGO, and a C compiler, but no Docker daemon. It does not demonstrate
recovery from a real dispatched restart. See the
[reconciliation design note](docs/action-reconciliation-design.md).

## Run automated checks

These checks need no Docker daemon:

```sh
test -z "$(gofmt -l cmd internal)"
go test ./...
go test -race ./...
go vet ./...
CGO_ENABLED=1 go build -o bin/aegis ./cmd/aegis
```

Tests run the real Docker SDK against a local fake HTTP API, including negotiation,
error responses, incomplete payloads, timeouts, and cancellation. They assert the
expected read-only requests and check that secret fixture values stay out of output.
Persistence tests use real temporary SQLite files, including close/reopen,
rollback, schema versions, ordering, and lock contention. They need a C compiler
and CGO enabled. They do not require a real Docker daemon.
Scheduling and retention tests use explicit times and a small wait-function seam.
Fake Docker endpoints test cancellation, timeouts, missing containers, and daemon
unavailability without stopping your Docker daemon. A child-process test verifies
that an abrupt kill releases the data-directory lock.

## Run the monitoring demonstration

```sh
bash scripts/smoke-monitor.sh
```

This live Docker demonstration starts a uniquely named sandbox and a real Aegis
process. Another process reads its history, a host marker switches the sandbox
health check from healthy to unhealthy, and Aegis stops through SIGTERM with exit
`0`. It verifies `healthy → suspect → persistent failure → recovering → healthy`
using distinct checks, confirms unchanged container restart count and execution
start, then restarts Aegis against the same SQLite file and checks that old
records remain while new ones are added. Cleanup stops its monitor, removes only
its Compose project, and deletes its private temporary files.

The script places its temporary directory under ignored `data/` because Docker
Desktop may not share `/tmp`. Docker Desktop must allow bind mounts from this
project. The marker is a synthetic demo control, not a real application liveness
probe. Aegis itself makes only read-only Docker requests.

For interactive commands and scoped cleanup, follow the
[manual monitoring demonstration](demo/README.md).

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
It saves each observation to a temporary SQLite file and reads it in a new process
with invalid Docker connection settings. This is a live Docker check, followed
by real SQLite persistence, rather than a fake API test.
Its exit trap removes only that project's containers and private temporary files,
including the smoke test database.
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

Save one observation and read it in a separate process:

```sh
container_id="$(docker compose -f demo/compose.yaml -p aegis-inspection-demo ps -q healthy)"
./bin/aegis inspect --container "$container_id" --save --db data/demo-observations.db
./bin/aegis history --container "$container_id" --limit 20 --db data/demo-observations.db
```

Remove the manual demo:

```sh
docker compose -f demo/compose.yaml -p aegis-inspection-demo down --timeout 5 --remove-orphans
```

The observation remains available after container removal. In the same shell:

```sh
DOCKER_HOST=invalid-Docker-host ./bin/aegis history --container "$container_id" --limit 20 --db data/demo-observations.db
```

The manual demo database remains in `data/demo-observations.db`. One-off saves do
not prune history. A monitor applies its configured retention to its database.

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
age but applies no freshness threshold to one-off inspection. Monitoring applies
the assessment freshness policy above. Saved history records earlier observations,
not current health. Neither command runs an independent probe or performs recovery.

Aegis displays only allowlisted metadata and its own explanations. It does not
print environment variables, health-check commands, check output, full inspection
JSON, or raw Docker error bodies. Docker sends the full inspection response to the
SDK in memory; this is not a filtered Docker endpoint. Container names and IDs
appear in output, so do not put secrets in names.

Docker socket access is privileged. Read-only inspection is a restriction in this
program's behavior, not a security sandbox or a restriction on socket permissions.
