# Draft release notes for v0.1.0-alpha

**Status: draft. Do not publish this release yet.** The project has no prior
version tags. This version is a proposed first public preview, not a production
readiness claim.

## Included

- Read-only inspection of one explicitly selected Docker container.
- SQLite observation and assessment history with schema migrations.
- Record-only periodic monitoring by default.
- Explicitly opted-in, bounded container restart recovery with durable budgets.
- Bounded diagnostic evidence with redaction, integrity checks, and retention.
- Conservative reconciliation and operator acknowledgment for uncertain actions.
- Optional direct dependency gates for recovery.

## Compatibility and operating requirements

- Linux Docker Engine on one host. Docker socket access grants powerful control
  over that Engine.
- Go 1.27 or newer and a C compiler for builds. SQLite uses CGO.
- Docker Compose v2 or newer for the demos.
- Recovery requires explicit startup and service permissions. Managed containers
  must use Docker restart policy `no`.
- SQLite schema 6 is current. A write upgrades schemas 1 through 5 atomically.
  Older Aegis binaries reject newer schemas.

## Known limits

- Docker and SQLite do not share a transaction. Aegis records durable intent and
  handles ambiguous restart requests conservatively, but it cannot promise
  exactly-once execution.
- Docker inspection and restart dispatch are not atomic. Stop Aegis before
  manually changing a managed container when you need a strict boundary.
- Log redaction is pattern-based and cannot identify every secret.
- Aegis supports one host and one active monitor per data directory. It cannot
  recover a failed host or unavailable Docker Engine.
- Dependency gates inspect only direct dependencies and do not prove network
  connectivity from the application.

## Validation status

The full milestone is incomplete. See [release-readiness.md](release-readiness.md)
for the recorded Go checks, clean-checkout Docker results, and remaining
acceptance gaps. Repeated comparison experiments remain outstanding. Do not
publish this draft until that evidence has been completed and reviewed.
