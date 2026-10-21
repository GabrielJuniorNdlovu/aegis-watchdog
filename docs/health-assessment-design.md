# Health assessment evidence and transitions

The existing adapter keeps aggregate Docker health and the latest completion time. Schema 2 stores those facts and collection time. Neither proves how many checks failed. Schema 3 adds execution start time, health-check configuration, and individual start, end, and exit-code fields. Old rows remain readable and cannot establish a streak.

A check belongs to a full container ID and Docker `StartedAt` execution. Its start and end timestamps identify it within that execution. Exit code zero succeeds; a positive code fails. Missing or negative codes are unusable. Commands, output, environment, and raw inspection bodies are never retained. Aggregate Docker health remains separate from these outcomes.

The evaluator receives explicit time, policy, previous state, and an observation or sanitized collection-error category. It processes completed checks by end time, then start time. A high-water mark prevents recounting older checks. The preceding response's identities distinguish overlapping history from newly arriving older evidence. Conflicting, overlapping, future, or newly out-of-order checks produce unknown.

Defaults are 3 failed checks, 2 recovery checks, 30 seconds maximum evidence age, and 30 seconds maximum gap between check completions. All settings must be positive. Age and gap exactly at their limits are usable. Start and end must be present, ordered, no later than collection or evaluation time, and belong to the current execution. Collection time must also be fresh. Only lifecycle `running` can establish application health.

Each monitor starts empty with a startup boundary. Checks must start at or after that boundary and complete after it. Replacement, restart, or changed policy starts a fresh state at the current evaluation time. Historical rows never restore streaks. This intentionally waits for a check that starts after the boundary.

A successful check establishes healthy unless persistent failure is active. Failures produce suspect until the failure threshold, then persistent failure. Success after persistent failure produces recovering until the recovery threshold. Failure during recovering returns to persistent failure. Counts describe consecutive **observed** checks, not proof that Docker's bounded log contains every execution.

Fresh duplicate reads preserve state and counts. Collection errors, stale data, missing checks, unusable timestamps, non-running lifecycle, or excessive gaps produce unknown, reset counts, and establish a new evidence boundary. A persistent-failure condition survives these gaps: later successes must still reach the recovery threshold. Replacement, restart, policy change, and process startup clear that condition.

Each monitoring result and its assessment commit in one SQLite transaction. Assessments retain policy, counts, execution identity, last evidence timestamp, and explanation independently of raw-row retention. Collection-error assessments have no container ID. History filters the exact requested selector and opens SQLite read-only. Every explanation states that no recovery action is authorized or executed.
