# Architecture

```text
   tracker / SCM webhooks        polling (reconciliation)
             \                        /
              v                      v
        +------------------------------------+
        | Inbox: authenticate, allow-list,   |   stores normalized events only;
        | dedupe by fact                     |   never starts work
        +------------------+-----------------+
                           v
        +------------------------------------+
        | Engine + workflow.Decide (pure)    |   the only writer of task state;
        | one atomic commit per event        |   owns all human gates
        +------------------+-----------------+
                           v  jobs
        +------------------------------------+
        | Scheduler: leases, heartbeats,     |   concurrency limits, retries,
        | limits, orphan cleanup             |   timeouts, transcripts
        +------------------+-----------------+
                           v
        +------------------------------------+
        | Pipeline: job handlers             |   report results as events
        +---+-----------+----------+---------+
            v           v          v
        IssueTracker  Source    AgentProvider ---> ExecutionRuntime
                      Control                      (isolated workloads)
```

Durable state lives in a `StateStore`. The control plane and the workloads are separate: the
orchestrator can run anywhere that can reach an engine, and nothing assumes they share a host.

## Rules that shape everything

- **The state machine is code, not a prompt.** `workflow.Decide` is a pure function. Models and
  repositories influence tasks only as data in job results.
- **Providers report facts.** Adapters get no store and no engine, and `internal/archtest` fails the
  build if they import them, or if a core package names a product.
- **Effects are idempotent.** Delivery is at-least-once everywhere. Events dedupe on the fact they
  report, jobs on deterministic keys, external effects on those same keys.
- **Credentials and untrusted code never share a workload.** See
  [ADR 0004](adr/0004-agent-credential-boundary.md) and [threat-model.md](threat-model.md).
- **Unsupported is a state.** Missing capabilities become a visible `blocked` reason, never a silent
  success.

## Where things are

| Concern                    | Package                 | Docs                           |
| -------------------------- | ----------------------- | ------------------------------ |
| Domain model and contracts | `internal/core`         | [providers.md](providers.md)   |
| States and gates           | `internal/workflow`     | [workflow.md](workflow.md)     |
| Control loop               | `internal/scheduler`    | [workflow.md](workflow.md)     |
| Job handlers and prompts   | `internal/pipeline`     | [workflow.md](workflow.md)     |
| Persistence                | `internal/storage`      | [workflow.md](workflow.md)     |
| GitHub                     | `internal/providers/github` | [github.md](github.md)     |
| Claude CLI                 | `internal/providers/claude` | [claude.md](claude.md)     |
| Docker and workspaces      | `internal/runtime`      | [docker-runtime.md](docker-runtime.md) |

## How it is tested

| Layer                          | What is real                                        | Command             |
| ------------------------------ | --------------------------------------------------- | ------------------- |
| Unit and contract suites       | Adapters against fakes and a fake GitHub            | `make test`         |
| Store conformance              | SQLite always; PostgreSQL with a DSN                | `make test`         |
| Acceptance (`test/e2e`)        | Everything except GitHub's API and the CLI binary, with real Git and restarts between steps | `make test` |
| Real binary                    | Mock workflow through HTTP, across restarts         | `make integration`  |
| Real containers                | Runtime contract, confinement, acceptance scenario  | `make docker-test`  |
| Compose                        | The shipped mock distribution                       | `make compose-smoke` |
| Real services (opt-in, costs usage) | Real CLI stream; real GitHub and Claude        | `test/smoke/real.sh` |
