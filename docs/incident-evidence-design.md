# Bounded local evidence

Capture is an internal operation for an existing reservation. It never changes
an action, creates a reservation, refunds budget, or calls a Docker mutation.
One capture record per action prevents silent replacement and unbounded revisions.
The live monitor performs evidence reconciliation and bounded cleanup only.

The version-1 JSON allowlist includes action/service/full container IDs, capture
times, effective capture limits, original reservation policy, up to four persisted
assessment summaries, selected lifecycle/start/check times, redacted recent logs,
and explicit issues and truncation. Environment, inspect bodies, probe commands,
probe output, operator acknowledgment text, and arbitrary diagnostics are excluded.
Database payload reads and row counts are bounded before decoding.

Defaults are a 10-second collection deadline, five-minute log lookback, 64 KiB
Docker log stream read allowance including framing, 512 KiB bundle, 16 MiB total
logical file storage, and seven-day evidence retention. Configuration validates
JSON's worst-case sixfold escaping plus a 64 KiB metadata reserve. A bounded
final recording step may take up to five additional seconds after cancellation.
Local filesystem operations can still block in the kernel.

Non-TTY logs use a tested streaming Docker frame decoder. TTY logs are raw.
Both reads and line processing are bounded. Lines over 4096 bytes and unfinished
last lines are omitted; they are never persisted before redaction. Common
Authorization headers and token/password/secret/key assignments are redacted.
Pattern matching cannot detect every secret. Log truncation or missing evidence
makes a partial capture; complete means complete only within the selected scope.
Inspection before and after logging detects disappearance, identity mismatch, or
execution changes without substituting a name-matched container.

The directory is fixed at evidence/ beside the selected database. It must be a
real directory with mode 0700; files are exclusive 0600 regular files. Internal
numeric action IDs generate all filenames. Descriptor-relative os.Root operations,
no-follow file opens, regular-file checks, and directory identity checks reject
unexpected symlinks and traversal. Temporary files contain only redacted JSON.
Publication uses an atomic no-replace hard link followed by temporary-name removal,
with file and directory synchronization. No existing bundle is overwritten.

SQLite records pending intent before collection. Complete, partial, or failed
status is recorded only after publication or a clear collection failure. A valid
published bundle left by a crash is checked against its pending row, schema,
limits, identity, and size before its result is adopted. An unpublished pending
capture becomes failed and its known temporary file is removed. Completed files
are checked for disappearance or corruption; inspection reports these conditions
without repairing them. Startup reconciliation records them durably. Unknown
files are never deleted and count toward the allowance.

Reservation-associated captures and action audit remain separate. A deletion
intent is persisted before cleanup unlinks a resolved action's expired bundle;
its retired timestamp records completion. Startup retries interrupted deletions.
Cleanup removes at most 32 expired resolved captures per pass. Unresolved actions
are always protected. Active registry and directory scans have a fixed 4096-entry
bound; exceeding it blocks work rather than assuming absent evidence. Storage
admission reserves a full maximum-size bundle, including the temporary file.
Temporary and published hard links share the same content during publication.
No protected evidence is deleted to admit another capture.
