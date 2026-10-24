# Interactive console

Build Aegis with Go 1.27 or newer and a C compiler, then launch the console:

```sh
CGO_ENABLED=1 go build -o bin/aegis ./cmd/aegis
./bin/aegis console --db data/aegis.db
```

The console displays the existing owl banner once and opens an `aegis>` prompt.
It requires interactive stdin and stdout and an ANSI-capable terminal. Use the
existing one-shot commands for pipes, files, and scripts. `aegis welcome` remains
available to preview the artwork independently.

## Example session

Replace `my-app` with an existing Docker container and `FULL_DOCKER_ID` with the
full ID returned by inspection:

```text
aegis> /help
aegis> /status
aegis> /inspect my-app
aegis> /inspect my-app --save
aegis> /history --container FULL_DOCKER_ID --limit 5
aegis> /decisions --container my-app --limit 5
aegis> /watch --container my-app
```

Press Ctrl+C to leave the watch and return to the prompt. `/exit`, `/quit`, or
Ctrl+D on an empty input line exits the console. Arrow keys edit input and recall
up to 100 commands from the current session. No history file is saved. Ctrl+C
at the prompt clears unfinished input.

If Docker uses a CLI context, select the SDK endpoint as for one-shot inspection:

```sh
export DOCKER_HOST="$(docker context inspect --format '{{.Endpoints.docker.Host}}')"
```

## Supported commands

- `/help`: console command list and input rules.
- `/inspect <container> [--save] [--db <path>]`: the existing bounded Docker
  inspection. Only an explicit `--save` writes the resulting observation.
  The existing `--container <name-or-id>` form also works.
- `/history --container <full-docker-id> [--limit <1-1000>] [--db <path>]`:
  stored observations for that exact container ID, labeled as historical.
- `/decisions --container <requested-name-or-id> [--limit <1-1000>] [--db <path>]`:
  the existing assessment history, including explanations, policy, and evidence
  ages. Use the same selector that the monitor recorded.
- `/errors --container <requested-name-or-id> [--limit <1-1000>] [--db <path>]`:
  stored collection failures, separate from application health.
- `/budget --service <logical-id> [--max-attempts <N>] [--budget-window <duration>]
  [--cooldown <duration>] [--limit <1-1000>] [--db <path>]`: existing budget
  inspection, including reservation IDs. These query options evaluate history;
  they do not change a running monitor's policy.
- `/actions --id <positive-action-id> [--db <path>]`: one existing action's intent,
  audit history, outcome, verification, and evidence metadata. There is no new
  action-list service; obtain reservation IDs from budget history.
- `/evidence <positive-action-id> [--db <path>]`: existing capture metadata and
  bundle file validation. Missing captures, partial captures, and unavailable
  files are reported. Raw logs and bundle contents are not displayed.
- `/status [--db <path>]`: the journal's resolved path and file existence. It
  neither opens SQLite nor probes Docker. Controller status is reported as
  unknown; there is no daemon connection.
- `/watch --container <requested-name-or-id> [--interval <duration>] [--db <path>]`:
  follow the newest stored assessment. The default poll interval is two seconds;
  accepted intervals are 200 milliseconds through one minute.
- `/exit` or `/quit`: leave the session.

The launch `--db` supplies the default path; individual commands can override it.
Input supports single quotes, double quotes, and backslash escapes. Shell
variables, substitutions, globs, pipes, and redirections are never evaluated.
Commands are limited to 4096 bytes and 64 arguments.

## Watch ownership and freshness

`/watch` opens the journal read-only for each poll. It does not collect Docker
observations, start monitoring, acquire a controller lock, or enable recovery.
Start a monitor separately with the existing `aegis monitor` command when needed.
The monitor owns its existing data-directory lock and lifecycle independently.
Exiting the console or canceling watch leaves that process running.

Watch prints the newest snapshot and subsequent changes, including transitions
from fresh to stale. Each displayed snapshot identifies its read time, collection
age, health-evidence age, and the recorded maximum evidence age. Missing or
future-dated timestamps are marked unavailable or unknown. Old snapshots remain
historical records. Freshness at the displayed read time does not establish that
a monitor is currently running or that application health is currently good.

An empty assessment history displays a waiting message. A query error ends watch
and returns to the prompt. The console does not create a missing journal.

## Output and operational boundaries

All application output passes through a console-only terminal-control escape
filter. This includes container data, database strings, paths, and error text.
Trusted banner colors and the line editor's cursor controls are handled
separately. `NO_COLOR=1` disables banner colors; cursor controls needed for editing
remain. Narrow terminals receive the existing compact or plain banner, and the
terminal wraps long result fields without truncating Docker IDs.

Each command, including a long-running watch, has a 64 KiB output allowance.
Reaching it prints a truncation marker and returns to the prompt. Existing query
limits still apply. Use a one-shot command when full output is required.

Ctrl+C cancels the foreground operation's context. Existing Docker and SQLite
deadlines remain in force. Cancellation does not undo an observation that has
already been committed. No Docker recovery operation can be dispatched through
the console.

Monitoring, recovery configuration, and operator acknowledgment remain available
through their existing one-shot commands. The console does not expose these
commands, a generic restart command, a shell, or an API server. In particular,
acknowledgment still requires its existing exact ID, operator reason, and stopped
daemon checks. Existing action and budget history are preserved.

The only added direct dependency is
[`github.com/ergochat/readline` v0.1.3](https://github.com/ergochat/readline), a
maintained Go line editor. It supplies editing, in-memory history, resizing, and
terminal restoration. No full-screen UI framework is used. Tab completion is not
implemented.

## Verification

```sh
go test ./...
go test -race ./...
go vet ./...
CGO_ENABLED=1 go build -o bin/aegis ./cmd/aegis
bash scripts/smoke-console.sh
```

The smoke test requires Linux PTYs, Python 3, GNU `timeout`, and the build tools.
It builds the current source into an isolated temporary directory and drives the
real console with keyboard input at 120×30, 80×24, and 60×20. It uses a fake
Docker HTTP API and its own SQLite journal and record-only monitor. It never
connects to a real Docker daemon or restarts a container.

The smoke assertions cover unchanged artwork, no startup writes or Docker calls,
editing, history recall, idle and foreground Ctrl+C, query-error recovery,
read-only watch, Ctrl+D, SIGTERM, terminal restoration, piped-input rejection, and
keeping the separate monitor alive. Cleanup is limited to its temporary files
and child processes. Automated PTY checks do not replace checking your own
terminal font and theme; preview those with `./bin/aegis console` and
`NO_COLOR=1 ./bin/aegis console`.
