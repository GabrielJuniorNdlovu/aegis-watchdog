# Milestone 10 verification pass

**Status: incomplete.** This pass validates the existing implementation. It does
not complete the comparison experiments or every end-to-end acceptance scenario.
The proposed `v0.1.0-alpha` release remains an unpublished draft.

## Code and environment

Verified the reported commits before changing anything:

- `6e08b6b`: recovery pre-dispatch clock fix.
- `429445e`: action-history and published-evidence assertions in the smoke test.
- `f086e7e`: MIT license, reviewer commands, and draft release notes.

The four Go checks ran against the production Go source at
`f086e7e23915007ae6b4d8e0e710030f2dfe9d26`. This pass changes no production Go
code. The smoke-test fix is `4639621fba5a323a542932fb8289630bcb8149eb`; the clean
checkout build and Docker acceptance runs use that commit. Later documentation
changes record the results without changing the executable or tests.

Environment observed on 2026-10-24:

- Ubuntu 26.04.1 LTS, Linux 7.0.0-38-generic, x86_64, glibc 2.43.
- Go 1.27.2, GCC 15.2.0, `CGO_ENABLED=1`, and `CC=gcc`.
- Docker CLI 29.9.1, Engine 29.8.2, and Compose 5.6.0.
- Docker context `desktop-linux`, using
  `unix:///home/ga3riel/.docker/desktop/docker.sock`.
- Demo image `busybox:1.37.0`, resolved to
  `busybox@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e`.

`go.mod` requires Go 1.27.0. CI reads `go-version-file: go.mod`, enables CGO, and
runs formatting, tests, race tests, vet, and build. The requirement was not
changed. A compiled C program verified the compiler and linker prerequisites.

No compatible Go executable was found. The official Go 1.27.2 archive was
extracted into `/tmp/aegis-go1272.Xu2mFMrb/go`, after verifying SHA-256
`ecbadb99091a3f46e31f5f934b068b1864eafa7995211b39eaddf76996045fe5` against
[the official release metadata](https://go.dev/dl/?mode=json&include=all).
This is a temporary toolchain, not a host installation.

The selected Docker socket existed with mode `0775` and was owned by the current
user. The sandbox denied socket access. Outside the sandbox, the same endpoint
was initially unavailable. After the operator made Docker Desktop ready, the
same context connected successfully. No context, socket permissions, or host
services were changed by the agent. `/var/run/docker.sock` was absent; it was not
the selected endpoint.
Docker host, context, API-version, and TLS environment overrides were unset.

## Checks and exit results

All four requested commands completed with exit `0`:

```sh
go test ./...
go test -race ./...
go vet ./...
CGO_ENABLED=1 go build ./cmd/aegis
```

The commands used the verified Go toolchain with `GOTOOLCHAIN=local`,
`CGO_ENABLED=1`, and `GOMAXPROCS=2`. Dependencies and build output used temporary
`GOMODCACHE` and `GOCACHE` directories. Each command had a 600-second execution
limit. The first test run downloaded pinned dependencies into the empty module
cache; it did not reuse an ignored project binary or a local database.

Additional checks completed with exit `0`:

- `go mod verify`.
- `test -z "$(gofmt -l cmd internal)"`.
- `bash -n scripts/smoke-inspect.sh scripts/smoke-recovery.sh`.
- `git diff --check`.

The full Go suite includes unit tests, temporary SQLite integration tests, fake
HTTP Docker APIs, and child test processes. The environment-gated live evidence
demo is skipped by ordinary `go test`; no claim is made that the unit command
runs every Docker scenario. `govulncheck` was not run in this pass.

Initial sandbox-only Docker access and a module-metadata lookup were blocked.
The same operations succeeded outside the sandbox. No required check remains
blocked by those restrictions at the end of this pass.

## Reproduced smoke-test defect

The old recovery smoke placed several independent SQLite databases in one data
directory. Evidence files are named by action ID in the sibling `evidence`
directory. Different journals therefore tried to publish `capture-1.json` over
an earlier scenario's evidence. Publication correctly refused the collision.

A stronger assertion reproduced the problem with exit `1`. The persistent-fault
journal contained two canceled `evidence_failed` reservations and only one
failed verification. The old assertion accepted that result as budget
exhaustion. An existing `capture-1.json` had a different container identity from
the persistent scenario's action 1. All five demo containers were removed by the
failure trap; diagnostic files remain ignored under
`data/aegis-recovery-demo.0bNNDswz`.

Commit `4639621` gives each scenario its own data directory. It requires all
three persistent-fault actions to reach verification and end with
`verification_deadline_unhealthy`, instead of accepting evidence failures. It
also checks SIGTERM exit status and verifies that a subsequent record-mode
process preserves one consumed reservation without another container execution.

The corrected working-tree smoke completed with exit `0`. Read-only SQLite
inspection independently found three failed verification outcomes and three
recorded new executions. The committed script also passed in the clean
checkout below. No assertion was removed or weakened to obtain a pass.

## Clean-checkout acceptance

The clean checkout completed these commands with exit `0`:

- `CGO_ENABLED=1 go build -o bin/aegis ./cmd/aegis`, under a ten-minute limit.
- `./bin/aegis inspect --help`.
- `timeout --signal=TERM --kill-after=30s 5m bash scripts/smoke-inspect.sh`.
- `timeout --signal=TERM --kill-after=30s 12m bash scripts/smoke-recovery.sh`.

The binary's Go build metadata recorded revision
`4639621fba5a323a542932fb8289630bcb8149eb` and `vcs.modified=false`. The final
checkout status was clean. The recovery smoke confirmed all three failed
verification outcomes under the stronger committed assertions.

The checkout was created by local Git clone with `--no-local --no-hardlinks`,
then detached at `4639621fba5a323a542932fb8289630bcb8149eb`. Before the build,
`git status --porcelain` was empty and `.vscode`, `bin`, and `data` were absent.
The checkout lives under ignored `data/` so Docker Desktop can bind-mount its
private controls. A new empty Go build cache prevents reuse of compiled project
artifacts. The shared temporary module cache contains pinned dependencies only.

Reproduce from the repository root with a compatible Go executable on `PATH`:

```sh
mkdir -p data
aegis_repo="$PWD"
aegis_checkout="$(mktemp -d "$PWD/data/aegis-checkout.XXXXXXXX")"
git clone --no-local --no-hardlinks "$PWD" "$aegis_checkout"
git -C "$aegis_checkout" checkout --detach 4639621fba5a323a542932fb8289630bcb8149eb
cd "$aegis_checkout"
export CGO_ENABLED=1 GOTOOLCHAIN=local GOMAXPROCS=2
export GOCACHE="$(mktemp -d /tmp/aegis-fresh-build-cache.XXXXXXXX)"
go build -o bin/aegis ./cmd/aegis
./bin/aegis inspect --help
timeout --signal=TERM --kill-after=30s 5m bash scripts/smoke-inspect.sh
timeout --signal=TERM --kill-after=30s 12m bash scripts/smoke-recovery.sh
# After both smoke tests succeed and finish their own scoped cleanup:
cd "$aegis_repo"
rm -rf -- "$aegis_checkout"
```

## Acceptance coverage and gaps

The real-Docker scripts build and run the actual Aegis executable. The tests
listed in the next section use different boundaries and do not substitute for
missing end-to-end fault injection.

### Real Docker scenarios in this pass

- Inspection of healthy, unhealthy, and no-health-check containers, with exit
  `0`, exact resolved IDs, and saved history read in another process without
  Docker connectivity. Covered by `smoke-inspect.sh`.
- Record mode leaves a persistently unhealthy container's execution unchanged.
- Controlled recovery establishes a new execution, records sustained successful
  checks, exposes action history, and publishes a nonempty evidence file.
  The smoke asserts a `complete` or `partial` status, a published reference, and
  the exact evidence file's presence. It does not inject corruption or require
  every capture to be complete; integrity and partial-status edge cases have
  separate integration tests.
- A persistent external fault survives three restarts, all three actions fail
  verification, and the three-attempt budget blocks further intervention.
- Unhealthy and no-health-check dependencies block before reservation, with
  zero consumed attempts. A recovered dependency is freshly reevaluated.
- A dependency failing after dispatch does not cancel verification in progress.
- SIGTERM exits successfully. A new record-mode process reconciles interrupted
  verification to uncertainty, preserves the consumed reservation, and does not
  repeat the restart. This is a graceful interruption during verification,
  not a controller crash during dispatch.

### Unit, database, fake-API, and child-process coverage

- Dependency change after evidence capture: the recovery test
  `TestDependencyExecutionChangeAfterEvidenceCancelsReservedAction` uses the
  actual coordinator, fake inspection at the Docker boundary, and real temporary
  SQLite. It verifies cancellation, no restart call, and one consumed attempt.
- Container replacement: assessment tests reset streaks on identity change;
  `TestReconciliationFakeDockerReadOnlyFailures` verifies a returned identity
  mismatch through the real SDK and HTTP boundary. Monitor tests keep separate
  histories for replacements.
- Durable budgets: store tests cover close/reopen, replacement, all reservation
  states, rollback, and a fresh child process. `TestBudgetClockSurvivesFreshProcess`
  verifies persisted clock and attempt history.
- Interrupted dispatch: `TestDispatchAndVerificationBecomeUncertain` seeds
  unfinished journal states and proves conservative reconciliation without a
  mutation-capable adapter. Restart protocol tests assert one exact-ID POST and
  bounded cancellation. These are not actual controller kills at dispatch.
- Uncertainty acknowledgment: `TestSimulatedActionAcknowledgmentDemo` invokes
  the CLI entry point in child test processes against a fake Docker API and a
  simulated verifying action. It preserves history and consumption, enforces
  ownership, and verifies idempotent acknowledgment. It does not dispatch a
  real Docker restart.
- Missing and partial evidence: CLI, file-publication, and fake-API tests cover
  pending, partial, failed, missing, invalid, and interrupted bundles without
  exposing full logs in action inspection.
- Retention: store and evidence integration tests preserve unresolved actions,
  their assessment summaries, budget history, and required evidence, including
  interrupted deletion and protected-storage exhaustion.
- Ownership: fake-API monitor tests reject a second owner in the same data
  directory. Kernel-lock child-process tests prove release after a process kill.
  This is per-data-directory ownership, not a lock across unrelated directories.

### Required scenarios still missing full end-to-end coverage

- Deterministic controller death during an in-flight dispatch, followed by a
  restarted executable and real Docker reconciliation without blind repetition.
  A seeded `dispatching` row is not proof of that crash boundary.
- Real container replacement between reservation and dispatch, with old-target
  invalidation and service-budget preservation through controller restart.
- A dependency becoming unhealthy after evidence capture and before dispatch
  through the full executable. The coordinator currently tests dependency
  execution replacement; pure evaluation separately tests unhealthy health.
- Recovery-enabled controller restart after budget exhaustion. Current live
  restart coverage resumes in record mode; persistent budget enforcement across
  cold starts is otherwise covered by database and child-process tests.
- Explicit acknowledgment after a real ambiguous Docker action. The current
  CLI process scenario uses a simulated unfinished action.
- Deliberately missing or partial evidence and unresolved-action retention
  through a long-lived real monitor and isolated Docker workload.
- Two independently launched production executables competing for the same
  data directory while using real Docker. Current ownership proof uses the
  monitor package, fake Docker, and separate kernel-lock process tests.

## Isolation, execution limits, and cleanup

Both smoke scripts use unique Compose project names and exact selected
container IDs. Recovery scenarios now have independent journals, evidence,
lock files, and stdout/stderr files. Successful runs remove their own temporary
data; failed recovery runs retain private diagnostics. Runtime data is ignored
by Git. The pre-existing `.vscode` files were preserved.

The demos disable competing restart policies and bound CPU, memory, and process
counts. The database dependency is observation-only. Cleanup stops only the
monitor PID started by the script and runs Compose `down` for that script's
project. No prune command or shared-daemon shutdown is used.

Scenario waits and Aegis Docker requests are bounded. This pass additionally
wraps the scripts in five-minute and twelve-minute total execution limits,
with a 30-second allowance after SIGTERM for cleanup. Docker CLI calls do not
have independent per-command deadlines; the outer timeout bounds the run.
These are synthetic marker-based liveness fixtures, not application-request
recovery benchmarks.

Label-filtered `docker ps -aq` checks found no remaining containers for any
project created by this pass:

- Failed regression: `aegis-recovery-demo-0bnndswz`.
- Corrected working-tree smoke: `aegis-recovery-demo-2ifioclu`.
- Clean-checkout inspection: `aegis-smoke-aegis-smoke-sbdks48j`.
- Clean-checkout recovery: `aegis-recovery-demo-wjy48iua`.

After checking the committed revision, clean status, build provenance, and all
four successful checkout commands, only the newly created
`data/aegis-checkout.61aW1sU4` directory was removed. The intentionally retained
failed-regression diagnostics remain ignored. The pre-existing image remains
cached. Temporary toolchain and check logs are outside Git under `/tmp`.
The Go command results are in `/tmp/aegis-go-checks._xrdjg44`; clean-checkout
results and build metadata are in `/tmp/aegis-fresh-checks.n4tzttge`.

## Remaining comparison and release work

No comparison trials, performance measurements, or controller-overhead numbers
were collected. The comparison runner, independent application request probe,
repeated trials, pinned baseline version or digest, and sanitized raw results
remain outstanding for Docker restart policies, Autoheal, and Aegis. Include
process exit, failed local liveness while running, a persistent fault, and a
dependency outage with healthy local liveness. Preserve failures and timeouts.
No advantage over the other tools is established by this pass.

The MIT license and draft release notes remain in place. Keep `v0.1.0-alpha`
unpublished until the missing acceptance evidence and comparison work are
reviewed. No release tag, binary publication, merge, deployment, or GitHub
release is part of this pass.
