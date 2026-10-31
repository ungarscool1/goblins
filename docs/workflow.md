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

## Design conversation

The planner runs before any code is written. Its input is the ticket and the conversation;
its output is one of two things, as JSON in a fenced `goblins-plan` block:

- **Questions**, which are posted as one grouped comment. The task then waits, for as long as
  it takes, at no cost. A reply from a permitted person resumes the same agent session.
- **A design** with required sections: scope, architecture, alternatives, constraints,
  implementation plan, tests, security considerations, out of scope. Goblins renders it,
  hashes it, stores it as an immutable revision and posts it with the approval command.

### What a model can and cannot do

Prompts guide the model; they are not the security boundary. These are enforced in Go:

- A model's output only ever becomes questions or a design. A reply that says "approved", or
  a design that contains `/approve-design 1`, is just text in a design that still needs a
  person. Comments by automation are ignored, so posting it does nothing.
- Implementation and publishing recheck, right before running, that an approval exists for the
  exact revision and hash, whatever the state machine scheduled.
- Malformed planner output is retried once, then blocks the task. It never reaches the ticket.

### Untrusted input

- Comments by people without `min_input_permission` are withheld from the model, cost no
  model run and cannot approve. Their count is mentioned to the planner.
- Tickets, comments and test output go into the prompt as data, inside a delimiter with a random
  value the content cannot predict, under a system prompt saying they are not instructions.
  This reduces prompt injection but cannot eliminate it. The boundaries above are what contain it.
- An `intake_label` restricts which items start work at all. Without one, an issue opened by
  someone with `min_input_permission` starts work. For public repositories, keep this in mind:
  the defaults mean strangers cannot spend your subscription.

### Trackers that cannot carry a conversation

If a tracker cannot accept comments, messages go to the project's notification provider
instead. With no provider, the task becomes `blocked` with the reason rather than waiting for
an answer nobody could give. Replies can be entered by anyone with shell access:

```bash
goblins comment --config goblins.yaml --task <task-id> --as alice --body "/approve-design 2"
```

The same rules apply to the content, and shell access on the installation stands in for
tracker permission. The name you give is recorded as the approver.
