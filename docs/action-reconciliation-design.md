# Interrupted action reconciliation

Existing reservations use planned, dispatching, verifying, uncertain, succeeded,
failed, and canceled. Only planned is created by production code. There is no
Docker mutation call or production transition to dispatching or verifying.
Every future executor must commit dispatching before issuing a mutation. Planned
therefore means no mutation was dispatched and can safely be canceled on startup.

Startup acquires the existing exclusive data-directory lock and retains it for
the monitor lifetime. Planned becomes canceled without Docker access. Dispatching
and verifying inspect only the recorded full container ID, under the existing
five-second request limit, then become uncertain. A changed execution start does
not establish causation. One healthy response does not establish sustained
recovery. Missing, replaced, unavailable, denied, malformed, and timed-out targets
remain uncertainty. No request is resent.

Existing uncertain and terminal rows are untouched by startup. Acknowledgment
keeps uncertain as the historical outcome and adds acknowledgment time and reason.
The unresolved index and budget query exclude acknowledged uncertainty, while
counting every reservation toward its applicable window. Other action states and
all original reservation fields remain unchanged.

Schema 5 adds acknowledgment fields and a small append-only action_audit table.
Each conditional state update and its audit entry commit together. Audit contains
old and new states, explicit time, stable reason, explanation, and allowlisted
inspection evidence or a sanitized collection-error category. Linked assessment
snapshots remain intact. Reservation and audit rows are never routine-pruned.
A repeated startup produces no additional transitions. A repeated acknowledgment
returns already acknowledged and preserves the original reason.

Startup processes each unfinished action independently. If persistence fails,
startup fails and releases the owner lock. Already committed transitions remain;
retry skips them and processes remaining work. Reservation transactions refuse
planned, dispatching, or verifying rows as startup_reconciliation_required.
This durable gate and the owner lock prevent acceptance during incomplete or
failed startup without trusting a persisted process-ready flag. With no unfinished
rows there is no startup work; uncertain rows still impose their separate block.
No live monitoring path creates reservations.

Acknowledgment acquires the same lock and accepts only an explicit uncertain ID
and a nonempty reason of at most 512 UTF-8 bytes without control characters.
It does not inspect Docker, prove success, refund attempts, or reserve another
action. Reconciliation and acknowledgment advance the existing clock high-water
mark monotonically in their transaction. Budget and cooldown checks remain active.
Inspection opens SQLite read-only and displays original intent, audit, evidence,
acknowledgment, and unresolved status. Simulated interrupted states are test
fixtures, not evidence of recovery from a real dispatched restart.
