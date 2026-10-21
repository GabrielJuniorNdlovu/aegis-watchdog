# Recovery reservation semantics

A reservation records a proposed attempt and consumes budget. It does not
establish restart eligibility or authorize a Docker operation. The live monitor
never calls the reservation operation. No public command creates, dispatches, or
executes reservations. Explicit acknowledgment can resolve an uncertain outcome without proving success.

## Explicit logical service identity

`store.Proposal` requires a logical `ServiceID` and the full target Docker ID.
The service ID is independent of a Docker selector. It contains 1 to 128 letters,
digits, dots, underscores, or hyphens, starting with a letter or digit. The first
accepted proposal registers it in `services`. Each reservation records the exact
container assigned to that service by the caller.

A replacement uses the same service ID and retains its budget. A new service ID
creates a separate budget. Reusing a container name performs no service mapping.
The caller must keep the configured logical ID stable. Monitoring and historical
observations do not implicitly register services.

All services on a host must use one canonical SQLite journal. Host-wide exclusion
means exclusion across that journal. Independent databases do not share budgets
or unresolved-action state.

## Budget and coverage boundaries

Defaults are three attempts in a one-hour rolling window with a five-minute
cooldown. Maximum attempts, window, and cooldown must all be positive.
`Reserve` receives explicit UTC evaluation time. It counts reservations in
`(now - window, now]`, regardless of state. An attempt exactly at the left boundary
is excluded. Cooldown ends exactly at `last reservation + cooldown`.

All committed reservations consume budget. Success, failure, cancellation, and
uncertainty never refund attempts. Planned, dispatching, verifying, and uncertain
states remain unresolved unless uncertainty has an explicit acknowledgment.
Any unresolved row blocks new reservations for every service in the host journal, even after its window or cooldown has elapsed.
Startup cancels interrupted planned actions without refunding attempts.
Dispatching and verifying become uncertain. No production executor transitions
actions into dispatching, verifying, succeeded, or failed yet.

On the first accepted reservation, the service's coverage starts at
`now - initial window`. That initial period is a known empty journal followed by
its first attempt. The coverage boundary never moves forward or resets when the
policy changes. A requested window starting before coverage is blocked as
`insufficient_history`. It becomes usable when time advances enough that its
left boundary reaches coverage. Equality is sufficient. A larger attempt limit
or different cooldown never erases previous attempts.

The journal retains all reservations, service coverage, and clock metadata.
Routine retention still prunes only observations, collection errors, and
assessments in bounded batches. It never deletes unresolved, terminal, or
budget-relevant reservation records. The journal therefore grows with attempts;
reservation archival is not implemented.

## Atomic writes and clock safety

`Reserve` validates its inputs and acquires the existing data-directory ownership
lock. Its transaction blocks unfinished planned, dispatching, or verifying rows
as `startup_reconciliation_required` until monitor startup reconciles them.
SQLite's immediate write transaction reads the journal, checks limits, and
inserts one planned reservation. A partial unique index independently permits
only one unresolved row. Separate connections cannot both pass the checks.
The journal itself is the attempt counter. Storage or commit failure returns an
error and rolls back registration, clock updates, and intent together.

A policy block returns a decision without a reservation and without an error.
A validation, ownership, database, or persistence failure returns an error.
A valid policy-blocked call can still advance durable clock metadata. It consumes
no attempt. Pure `budget.Evaluate` receives explicit facts and time and performs
no I/O.

Each committed reservation evaluation persists the greatest time seen by
reservation calls or existing journal records. Backward tolerance is zero: even one
nanosecond behind durable timing evidence blocks reservations as `clock_unsafe`.
Clock state survives process restarts. A future reservation remains timing
evidence even when it falls outside the current counting interval. New proposals
remain blocked until time reaches that evidence, then all other limits still
apply. Read-only inspection does not advance the clock high-water mark.

An optional assessment reference must resolve to the exact target Docker ID.
The reservation copies the allowlisted assessment snapshot into its own row.
That snapshot remains authoritative after routine assessment pruning; the source
sequence is diagnostic, not a foreign-key dependency or a later lookup.
Evidence freshness and other recovery prerequisites are not evaluated here.

## Read-only inspection

`aegis budget --service <id>` opens SQLite with `mode=ro` and takes a consistent
read transaction. It does not connect to Docker, create files, register services,
perform migrations, or modify clock metadata. It displays the selected evaluation
policy, attempt count, cooldown end, history coverage, timing evidence,
host-wide unresolved reservation, and reservation history with each saved policy.
The history limit affects displayed rows, not accounting.

The command's policy flags select a read-only evaluation. They do not update the
policies saved in reservations. The defaults are `--max-attempts 3`,
`--budget-window 1h`, and `--cooldown 5m`. A blocked budget or empty history returns
zero because inspection succeeded. Invalid arguments return 2. Read failures
return 1. Output says `budget available`, never that a restart is permitted.

Schema 4 added reservations. Schema 5 adds acknowledgment fields and atomic audit
records. Versions 1 through 4 remain readable without modification. Versions
before 4 have no reservation coverage. The next write migrates them atomically.
Newer schemas are rejected.

Startup reconciles unfinished actions under the owner lock. An uncertain action
blocks reservations until `aegis actions acknowledge` records an explicit operator
reason with the daemon stopped. Acknowledgment preserves budget and uncertainty.
Action audit and acknowledgment history are never routine-pruned. See
[action reconciliation](action-reconciliation-design.md).
