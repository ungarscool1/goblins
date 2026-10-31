# Workflow

The workflow is a deterministic state machine in `internal/workflow`. `Decide` is
a pure function: `(task, event, facts, policy) -> (next state, jobs)`. The engine
looks up facts (the actor's permission) first, runs `Decide`, then commits the
result atomically. Models and repository content influence a task only as data
inside job results.

## States

| State              | Meaning                                                 |
| ------------------ | ------------------------------------------------------- |
| `discovered`       | Accepted item, before its first job                     |
| `investigating`    | Planner job queued or running                           |
| `waiting-for-user` | Planner asked questions, idle until a permitted reply   |
| `design-proposed`  | A revision awaits approval, idle until a permitted reply |
| `agent-ready`      | Exact revision approved, implementation queued          |
| `implementing`     | Implement, validate and publish jobs                    |
| `pr-open`          | Draft pull request exists, review queued                |
| `reviewing`        | Independent review running                              |
| `testing`          | Final validation of the pushed commit                   |
| `completed`        | Report posted; nothing was merged                       |
| `blocked`          | A person must act; the reason is on the task and ticket |
| `quota-paused`     | Usage limit or login required; resumes on a timer       |
| `failed`           | Unrecoverable; reported on the ticket                   |
| `cancelled`        | Closed or cancelled by a permitted person               |

## Transitions

| From               | To                                                         |
| ------------------ | ---------------------------------------------------------- |
| `discovered`       | `investigating`, `reviewing` (review-only tasks)           |
| `investigating`    | `waiting-for-user`, `design-proposed`, `investigating`     |
| `waiting-for-user` | `investigating`                                            |
| `design-proposed`  | `agent-ready`, `investigating`                             |
| `agent-ready`      | `implementing`, `investigating` (requirements changed)     |
| `implementing`     | `pr-open`, `investigating` (requirements changed)          |
| `pr-open`          | `reviewing`                                                |
| `reviewing`        | `testing`, `completed` (review-only tasks)                 |
| `testing`          | `completed`                                                |
| any active         | `blocked`, `quota-paused`, `failed`, `cancelled`           |
| `blocked`, `quota-paused` | the state recorded when they were entered, or `investigating` if requirements changed meanwhile |

`CheckTransition` enforces the table on every commit, so a bug in the machine stops
a task instead of corrupting it.

## Invariants

1. **No implementation job exists before approval.** Only `onApproval` enqueues the
   first one, and only from `design-proposed`.
2. **Approval binds to one exact design.** It records revision, hash, approver and
   comment. It requires a human actor, sufficient tracker permission, a *newly created*
   comment *written after* that revision was proposed, and the command alone on the
   first line. Edits, quotes and prose do not count.
3. **Approval is voided by material change.** A new design revision or a changed
   title or body clears it. Labels and timestamps are not material.
4. **Authorization fails closed.** A lookup that fails permanently counts as no
   permission. A transient failure retries the event rather than rejecting a person.
5. **Automation cannot act as a person.** Comments by bots, unknown actor types and
   configured `BotLogins` are ignored, which also prevents loops on our own comments.
6. **Only the current job advances a task.** Outcomes of superseded or cancelled
   jobs are ignored, and a worker that lost its lease cannot record anything.
7. **Each event's effects are atomic.** Marking the event processed, the state change,
   the audit record, the design and the new jobs are one transaction.
8. **All loops are bounded.** Planning and implementation rounds have limits. A person
   saying `/goblins resume` resets them.
9. **Waiting costs nothing.** Idle states run no job, for as long as it takes.

## Delivery semantics

Delivery is at-least-once; effects are exactly-once *by idempotency*:

- Events dedupe on `DedupKey`, which identifies the reported fact.
- Processing is serialized per subject. Events of one issue are handled in arrival
  order, one at a time, under a lease.
- Jobs have deterministic idempotency keys derived from the task, passed on to
  external systems. A retried comment or pull request does not duplicate.
- Jobs are leased with heartbeats and fenced by `(owner, attempt)`. A job whose workers
  keep dying fails with an outcome event, so it cannot stay stuck.

## Commands

Commands must be alone on the first non-blank line of a new comment, from a person
with at least `MinApprovePermission`:

| Command                 | Effect                                                |
| ----------------------- | ----------------------------------------------------- |
| `/approve-design <rev>` | Approve that revision; a hash prefix may follow        |
| `/goblins pause`        | Stop the running job and block                        |
| `/goblins resume`       | Return from `blocked` or `quota-paused`               |
| `/goblins cancel`       | Stop everything and clean up                          |

## Storage

SQLite suits single-node installations; PostgreSQL suits several replicas. Both run
the same conformance suite (`internal/storage/storetest`), and the PostgreSQL run needs
`GOBLINS_TEST_POSTGRES_DSN`. Migrations are forward-only, are applied at startup and
refuse a database newer than the binary. See `internal/storage/migrations`.

Back up by copying the SQLite file while stopped, or with `pg_dump`.

## Scheduler

`internal/scheduler` runs the loop that connects everything:

1. **Inbox.** Webhooks and polls produce events. The inbox stores those whose source
   and container have a route, which makes routes the allow-list. Storing an event
   never starts work; it must still pass through the workflow.
2. **Reconciliation.** Each source can be polled at its own interval, resuming from a
   stored cursor. Events are stored before the cursor moves, so a crash re-reads and
   dedup absorbs it. This also serves deployments with no inbound connectivity.
3. **Engine.** Events are processed in order, per subject.
4. **Dispatch.** Jobs are claimed under leases and limits, then run with heartbeats.

### Limits

Limits apply to heavy jobs, which are those with a limit key such as an agent account.
Comments and notifications never wait for a long agent run.

### Failure handling

| Handler result                                | Effect                                    |
| --------------------------------------------- | ----------------------------------------- |
| Success                                       | Outcome event, workflow continues         |
| `transient`, `timeout` or `internal`          | Backoff and retry, then the task fails    |
| `quota_exhausted`, `auth_required`            | Task moves to `quota-paused`              |
| `unsupported`, `forbidden`, `invalid`, others | Task moves to `blocked` with the reason   |
| Lease lost or job cancelled                   | Attempt stops, its result is discarded    |
| Process shutting down                         | Job requeues at once, no failure recorded |

`JobTimeout` catches work that still heartbeats but never finishes. Orphan workloads are
removed at startup and periodically: a workload labelled with this installation's name
survives only while its exact job attempt holds a valid lease. Workloads of other
installations are never touched.

Worker output goes to a per-task transcript that is bounded per job and per line, and is
redacted before it is stored.
