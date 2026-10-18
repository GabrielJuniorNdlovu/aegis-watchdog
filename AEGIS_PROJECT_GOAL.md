# Aegis — Project Goal and Engineering Contract

## 1. Purpose

Build Aegis, a single-host Docker recovery controller that makes limited, explainable recovery decisions and preserves its recovery history across crashes.

Aegis monitors explicitly selected services, records evidence of persistent failures, decides whether a restart is permitted, captures diagnostic context, and verifies useful recovery afterward. It must be equally clear about why it declines to act.

The intended audience is operators of small Docker Compose deployments. The project is also a software engineering portfolio piece for an MLH Fellowship application: its value should be demonstrated through understandable code, reproducible failure tests, and honest comparisons.

**Success means a reviewer can reproduce a failure, inspect the decision, and verify the controller's guarantees.** It does not mean supporting the largest number of features.

This document describes desired behavior, not features that already exist. Do not claim novelty, production readiness, exactly-once execution, or guaranteed application success.

## 2. Product hypothesis

Basic container restarting already exists. Aegis is worth building if a small, understandable implementation offers practical benefits through:

- Evidence captured before intervention changes the system.
- Recovery budgets that survive daemon restarts and container replacement.
- Conservative handling of interrupted or ambiguous Docker operations.
- Explanations tied to the observations and policy that produced them.

Validate those benefits against correctly configured Docker restart policies and an existing Autoheal tool. If the results show little benefit, report that openly. Do not make competing tools look worse by giving them inappropriate health checks.

## 3. Scope and architecture

Use one Go executable, one active daemon, SQLite, and a local evidence directory on one Linux Docker host. Build internal modules only as implementation needs emerge; do not create an empty framework or a microservice for every responsibility.

Initial data flow:

```text
Validated configuration
        |
        v
Docker inspection -> timestamped observations -> pure policy evaluation
                                                      |
                                                      v
                                                  SQLite
                                                      |
                                                      v
                                              read-only CLI
```

Recovery adds this path only after its protections are implemented and tested:

```text
Eligible decision
    -> persist intent and reserve budget
    -> capture bounded evidence
    -> revalidate target and eligibility
    -> persist dispatch state
    -> request restart once
    -> verify health or record uncertainty
```

### Components

- **Configuration:** explicit service selection, health semantics, thresholds, dependency gates, and action permissions. Validate before monitoring starts. Configuration changes require a daemon restart in v1.
- **Docker adapter:** bounded inspection, log retrieval, and eventually restart calls. No arbitrary commands or generic Docker proxy.
- **Monitor:** initially one polling loop with per-request timeouts and a bounded cycle. Introduce concurrent polling only when measurements justify it.
- **Policy evaluator:** receives explicit observations, prior state, policy, and time. Returns a decision with reason codes and a human-readable explanation. No Docker calls, database writes, real-clock reads, or randomness inside evaluation.
- **Store:** SQLite migrations, transactional state changes, incident history, budget accounting, and read-only queries.
- **Recovery coordinator:** evidence collection, dispatch, verification, and reconciliation of unfinished actions.
- **CLI:** configuration validation, monitoring, status, and incident inspection. In v1, inspection commands open SQLite read-only and do not alter daemon state.

Use straightforward Go functions and types. Introduce interfaces at actual external boundaries that need substitution in tests; avoid general plugin systems.

### Deferred architecture

A local management API, live pause/resume commands, webhook notifications, policy replay, Docker event streaming, and a React/TypeScript dashboard are later additions. They are not prerequisites for v1 and should receive no placeholder implementations.

When a dashboard is added, it must use an authenticated management API; it must never access Docker or SQLite directly. Loopback binding alone is not authorization.

## 4. First milestone: read-only monitoring

Deliver a complete, usable path before implementing recovery:

1. Select one running container explicitly through validated configuration.
2. Inspect Docker lifecycle and health status with timeouts.
3. Record observations and explained decisions in SQLite.
4. Inspect the latest recorded state through the CLI.
5. Restart the daemon and confirm history remains available.

Read Docker's existing health-check status first. Distinguish `starting`, `healthy`, `unhealthy`, no configured health check, and failed inspection. No health check means unknown health, not healthy. A failed Docker request means an unavailable observation, not an unhealthy application.

Do not add a second HTTP probing system in this milestone. Repeated inspection of the same Docker health-check result must not count as multiple independent failures. Track the check timestamp or equivalent observation identity.

Record mode is the default. This milestone must contain no reachable automatic restart path.

## 5. Service identity and observation contract

A logical service has a stable, explicitly configured identity. Its current container has a Docker ID that may change during replacement.

- Selectors must resolve to at most one container. Ambiguity blocks intervention.
- Zero matches means absent or unknown; Aegis does not create or start containers.
- Recovery budgets belong to the logical service, not a replaceable container ID.
- A replacement invalidates pending actions for the old container and resets health streaks, but preserves the service's budget history.
- Record container ID, observation time, check time where available, health/lifecycle state, and collection errors.
- Define a maximum observation age. The CLI must show observation age and stale state instead of presenting old health as current.
- Thresholds must use distinct observations. A gap in fresh observations breaks a consecutive failure or recovery streak.
- Use UTC for persisted timestamps and monotonic elapsed time for in-process deadlines. Detect significant backward wall-clock movement and pause automatic recovery until timing is trustworthy; never silently replenish a budget because time moved backward.

The same observation sequence, initial state, policy, and explicit times must produce the same policy decisions.

## 6. Recovery policy and precedence

A Docker health check is application-defined. Before enabling recovery for a service, require an explicit configuration declaration that its check represents local liveness and that restarting is an appropriate response. Aggregate readiness or dependency checks alone do not justify a restart.

Evaluate these gates in order and record the applicable blocking reasons:

1. Configuration and service selection are valid.
2. The service is explicitly opted into recovery, and the daemon is running in recovery mode.
3. Startup reconciliation has completed and there is no unresolved action for the service.
4. The selected container is running and is not paused, stopping, or already restarting.
5. Observations are fresh, belong to the current container, and include usable liveness evidence.
6. Startup grace has elapsed and the persistent failure threshold is met.
7. Any explicitly configured dependency gates permit intervention.
8. Persistent recovery budget, cooldown, and the host-wide action limit permit intervention.

If all gates pass, produce an eligible decision. The executor must revalidate relevant gates immediately before dispatch.

### Dependency behavior

Dependencies are optional, explicitly configured recovery gates, not inferred root causes. Reject missing references and cycles. In v1, a configured gate blocks recovery while the dependency is unhealthy or unknown. Explain that this conservative policy may delay recovery from a simultaneous local fault.

A healthy dependency does not prove connectivity from the application, and an unhealthy dependency does not prove it caused the application failure. Do not claim otherwise.

Use appropriately designed liveness checks first. Do not manufacture a dependency-restart problem through deliberately incorrect checks.

### Action limits

- Restart is the only automatic action.
- Target only the exact, currently running, opted-in container ID.
- Allow at most one in-flight action across the host in v1.
- Enforce a configured rolling attempt budget and minimum cooldown per logical service.
- Persist and count reservations conservatively, including uncertain attempts. Do not automatically refund canceled reservations in v1.
- Automatic recovery remains off by default and requires an explicit startup flag as well as service-level permission.
- To disable intervention in v1, stop the daemon or restart it in record mode. Live management commands are deferred.

Do not delete containers, start stopped containers, execute commands inside them, or automatically repair databases. The demo database is observation-only.

## 7. Durable action lifecycle

Keep observations, incidents, and actions separate. A service can remain unhealthy while action is blocked by budget or uncertainty.

Use a small explicit action lifecycle, for example:

`planned -> dispatching -> verifying -> succeeded | failed | uncertain | canceled`

Rules:

- Commit intent, target identity, policy version, and budget reservation in one transaction before any restart request.
- If durable recording fails, do not intervene.
- Persist `dispatching` before calling Docker. Do not automatically retry the restart HTTP request after a timeout or connection loss.
- Docker accepting a restart is not proof of application recovery.
- After startup grace, require a configured streak of distinct healthy observations within a verification deadline.
- If verification cannot establish success, record failure or uncertainty according to the available evidence. Never report success merely because a timeout elapsed.
- Finish startup reconciliation before considering new actions.
- Reconcile unfinished work using exact container identity, recorded lifecycle information, and current observations. If the outcome cannot be established, mark it uncertain and block further automatic action for that service.
- Document a deliberate operator resolution procedure for uncertain actions before shipping recovery mode. Preserve the original journal and budget history; never recommend deleting SQLite to clear the block.

SQLite and Docker do not share a transaction. A process can die between dispatch and recording its result. Aegis promises durable intent and conservative handling of ambiguity, not exactly-once restart execution.

## 8. Persistence and evidence

Use SQLite with migrations and transactions. Choose and document journal, synchronization, and lock-wait settings suitable for process-crash recovery and concurrent read-only CLI access. Enforce one daemon per data directory using a lock released by the operating system when the process exits.

Persist service identity, observations, policy versions, incidents, action history, and verification outcomes. Add only fields required by implemented behavior.

Before dispatch, capture a bounded diagnostic bundle containing recent logs, relevant health results, allowlisted metadata, and the decision explanation.

- No environment-variable dump or unrestricted Docker inspection payload.
- Log contents may contain secrets. Apply redaction, restrict file permissions, and document that redaction is not a complete guarantee.
- Bound collection time, bytes per incident, and total evidence storage.
- Publish completed files atomically where practical and record missing or partial evidence explicitly.
- Partial evidence may permit recovery only if the decision and evidence failure are durably recorded. Failure to persist the action journal always blocks recovery.
- Define retention before enabling recovery. Never remove unresolved actions or records still needed to enforce active budgets.

## 9. Operating boundaries

Run on a local Linux Docker host. Start as a foreground process; add a systemd unit once shutdown and persistence behavior are tested. Docker Compose runs the demo workloads, not a required production database for Aegis.

Docker socket access is privileged. A read-only socket mount does not restrict Docker API operations to reads. Record mode is a behavior restriction, not a security sandbox.

Do not expose a remote management listener in v1. Treat configuration as trusted operator input, validate it, and avoid accepting arbitrary commands.

Define and test a supported relationship with Docker restart policies. In the recovery demo, disable competing restart controllers for opted-in application containers. Refuse unsupported combinations rather than silently racing another controller.

Aegis does not restart stopped containers. An inspection and a restart request cannot be atomic with an operator's concurrent Docker command. Recheck immediately before dispatch, and document that operators should stop Aegis before manually manipulating managed containers when they require a strict guarantee against races.

Aegis cannot recover a dead host or a completely unavailable Docker daemon. Report uncertainty and use bounded retry/backoff for inspection.

## 10. Implementation milestones and completion gates

### A. Read-only vertical slice

Implement the path in section 4, a minimal health-check demo container, and focused persistence/policy tests. Demonstrate history surviving a daemon restart. No recovery writes to Docker.

### B. Recovery protections, with execution still disabled

Implement durable reservations, budgets, cooldowns, single-daemon ownership, action states, evidence limits, and startup reconciliation. Test them with a fake Docker adapter and injected process/storage failures. Define the uncertain-action resolution procedure.

**Gate:** protection tests pass before a real automatic restart path is enabled.

### C. Opt-in recovery

Connect the tested coordinator to Docker restart operations. Add real verification, immediate target revalidation, supported restart-policy checks, and a local failure sandbox. Keep record mode the default.

**Gate:** the recovery acceptance scenarios below pass using real containers, with limitations documented.

### D. Evaluation and release documentation

Complete the API/worker/database demo, comparisons, cleanup instructions, architecture notes, and reproducible commands. Add a systemd example if validated. Report actual measurements and unresolved limitations.

Dashboard, API, notifications, replay, event streaming, resource-based diagnosis, and multi-host support require a separate justified scope decision after these milestones.

## 11. Required acceptance scenarios

- A transient failure does not restart a service.
- Re-reading one failed Docker health result does not satisfy a multi-observation threshold.
- Missing health checks, stale data, and Docker inspection errors produce unknown or stale state and no recovery.
- A persistent local hang with permitted policy produces evidence, one restart attempt, and verified sustained recovery.
- A configured dependency outage blocks the dependent service's action with an explicit explanation.
- An unrecoverable application exhausts its attempt budget; restarting Aegis or replacing the container does not replenish it.
- Crashes before dispatch, during dispatch, and during verification are reconciled conservatively without blind request repetition.
- A replaced container does not receive an action targeting its predecessor.
- A stopped or paused container is not restarted by a newly evaluated action; the documented concurrent-operator race is acknowledged.
- Failed journal writes prevent intervention; partial evidence follows the documented policy.
- A second daemon cannot take control of the same data directory.
- Graceful shutdown stops scheduling new work and leaves any unfinished action recoverable from durable state.
- Backward clock movement does not silently expire recovery history or authorize new actions.

Use pure unit tests for policy and timing, temporary SQLite integration tests for persistence, and real Docker integration tests for lifecycle behavior. Inject crashes at meaningful boundaries. Do not replace all integration testing with mocked success responses.

## 12. Demonstration and comparison

Use isolated local demo workloads with bounded resource use and failure controls unavailable outside the sandbox. Scope cleanup to demo resources; never use broad Docker prune commands.

Compare Docker restart policies, an existing Autoheal implementation, and Aegis under the same documented scenarios. Identify tool versions, configuration, host conditions, repetitions, and measurement definitions.

Report:

- Recovery time from injected failure to sustained successful application requests.
- Restart counts, classified against known injected faults rather than assumed causal diagnosis.
- Budget behavior after controller crashes and container replacement.
- Evidence captured before intervention.
- Setup effort and measured resource overhead.

Label unavailable measurements and unsuccessful trials. If a simpler tool performs equally well, say so.

## 13. Instructions to the implementation agent

Inspect existing files and relevant repository instructions before making changes. Check Go and Docker availability. Do not assume the directory is a Git repository or claim checks ran when they did not.

Begin with milestone A. Deliver working increments, explain the decisions, and verify each milestone before adding the next. Ask only when a missing decision materially blocks progress; otherwise use the simplest documented choice consistent with this contract.

Use relevant skills selectively. Avoid generating multiple competing designs, large package scaffolds, or unused abstractions. Prefer the standard library; add a Docker client or SQLite dependency where it materially reduces complexity and justify the choice.

For each milestone report:

- What works and the files that matter.
- Exact run and test commands.
- Which checks actually passed and which were blocked.
- Important tradeoffs, known limits, and the next unfinished milestone.

Keep the implementation explainable to the project owner. Do not fabricate benchmark results, bypass failing tests, or describe planned features as implemented.

## 14. Definition of done

The first release is complete when a reviewer can follow the README to run the sandbox, inspect recorded decisions, enable recovery explicitly, reproduce the required failure scenarios, verify durable budgets and crash handling, and repeat the comparison experiments.

The release must include tests, sample configuration, bounded evidence handling, an operator procedure for uncertainty, documented limitations, and safe demo cleanup. A frontend is not part of this completion gate.
