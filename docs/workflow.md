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
| `awaiting-review`  | Report posted; idle until something happens on the pull request |
| `completed`        | The pull request was merged, or the report was the end (`stop_at_report`) |
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
| `pr-open`          | `reviewing`, `testing` (commit already reviewed)           |
| `reviewing`        | `testing`, `implementing` (findings to correct), `awaiting-review`, `completed` (review-only tasks) |
| `testing`          | `awaiting-review`, `completed` (`stop_at_report`)          |
| `awaiting-review`  | `reviewing` (new head), `implementing` (actionable feedback), `completed` (merged), `cancelled` (closed) |
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

## Not implemented yet

Blocking a task on another issue it depends on is **not part of P0**, although #4 lists
it. Each task proceeds on its own, so sequence dependent issues by hand. Dependency
planning is tracked in #25.

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

These also work in a comment on the pull request of a task.

Commands must be alone on the first non-blank line of a new comment, from a person
with at least `MinApprovePermission`:

| Command                 | Effect                                                |
| ----------------------- | ----------------------------------------------------- |
| `/approve-design <rev>` | Approve that revision; a hash prefix may follow        |
| `/goblins pause`        | Stop the running job and block                        |
| `/goblins resume`       | Return from `blocked` or `quota-paused`               |
| `/goblins cancel`       | Stop everything and clean up                          |

## What a blocked ticket shows

When validation blocks a task, the ticket comment includes the end of each failed check's
output, up to 4 KiB per check. The ticket can be public, so credential-shaped text is
redacted first. Redaction only recognizes common shapes, and validation workloads receive
no credentials: keep secrets out of what your checks print. Notifications and the task's
stored reason carry only the summary.

## Review

A review looks at one exact commit, once per commit and prompt version. Reviewers are tried in
order: the project's `review` provider, then each of `stages.reviewers`. No reviewer is required by
the code, and none is assumed.

| Situation                                        | Effect                                        |
| ------------------------------------------------ | --------------------------------------------- |
| Reviewer has no login, no quota or lacks a feature | The next reviewer takes over                |
| Reviewer keeps crashing                          | Retried, then the next one takes over         |
| None left, and `allow_skipped_review`            | Continues; the review is reported as skipped  |
| None left, no opt-in                             | Pauses or blocks for the reason of the first  |
| Finding at or above `review_min_severity`, not low confidence | Correction round, within the approved design |
| Below that                                       | Published as a suggestion                     |
| `changes_requested` with no finding              | Blocks: there is nothing to act on, but it is a gate |
| Unreadable review                                | Retried, never a pass                         |

A correction goes back to the implementer with the findings as data. Checks run on the new commit,
it is pushed to the same pull request, and that commit is reviewed. `max_review_fix_loops` (default
3) bounds this, after which the task blocks with the findings on the ticket. Findings cannot widen
the design: implementation still rechecks the approval.

Automated reviews are published as comments and never as an approval or a change request, so they
can neither satisfy nor block a person's branch protection, and each one says that it proves
nothing about safety. Goblins' own reviews do not trigger anything, as they come from automation.

## After the report

A task follows its pull request until someone merges or closes it, which ends the task and removes
its volumes. `policy.stop_at_report: true` ends it at the report instead.

Each commit gets one status comment on the ticket: the pull request, the commit, the validation result
and what stands between it and a merge. Later comments only come when that verdict changes.

**Signals only prompt a look.** Any webhook or poll about the pull request, once deduplicated, queues
one `reconcile` job. It asks the SCM where things stand now: head, reviews, threads, checks, changed
files and branch protection. It runs no model. So a late, replayed or forged-looking delivery can do
no more than cause a read, and webhooks and polling behave alike. Own remarks are recognized and
ignored. Remarks by anyone below `min_input_permission`, or by automation not listed in
`trusted_review_bots`, are withheld at this point, so they reach neither the workflow nor a prompt.

**Findings** are normalized (path, line, severity, confidence, provenance, commit) and keep a stable
ID, so seeing one again changes nothing. A remark on lines the head no longer has becomes `stale`: it is no longer work to do, but it holds up
the merge gate until its reviewer resolves it.

| What the reconcile finds                | What happens                                                    |
| --------------------------------------- | --------------------------------------------------------------- |
| New remark or change request text       | Correction round on the same pull request                       |
| Failed check on this commit             | One correction round per commit                                 |
| A head Goblins did not push             | Reviewed once. Validated from a fresh checkout of that commit   |
| Merged / closed                         | `completed` / `cancelled`, volumes removed                      |
| Nothing to act on                       | The merge policy is evaluated, and people told if the outcome changed |

Corrections share `max_review_fix_loops` with automated reviews, and being sent to the implementer
does not resolve anything: a finding stays `addressed`, and counts as unresolved, until the thread is
resolved on the SCM. A branch that moved during a correction blocks the task, and resuming looks again.

### Merge policy

The gate is a pure function (`EvaluateGate`) reporting what stands between a commit and a merge:

- the design revision is approved, and this commit passed validation;
- no check failed or is running, and required checks reported (`required_checks`, plus what branch
  protection requires);
- no person has an active change request, and no remark by a person is unresolved;
- `min_human_approvals` (default 1) people approved **this commit**, with `min_approve_permission`, on
  the `approvers` list if there is one, and not being the author. Automation never counts;
- with `require_branch_protection`, the base is known to be protected. Unknown is not protected.

A change touching a sensitive area (authentication, cryptography, network, automation, plus
`sensitive_paths`) is always reported as needing a person.

**Goblins does not merge**, unless a project lists an `auto_merge` rule. A rule applies only when the
whole change is inside its paths and touches nothing sensitive, and it cannot be configured to do
without a person: it needs an approval of this commit, passing checks, a known set of files, and an
SCM that merges exactly that commit. Passing tests and an automated review alone can never merge.

```yaml
policy:
  trusted_review_bots: [some-reviewer[bot]]
  merge:
    required_checks: [test]
    min_human_approvals: 1
    approvers: [alice, bob]
    require_branch_protection: true
    sensitive_paths:
      billing: ["internal/billing/**"]
    auto_merge:
      - {name: docs, paths: ["docs/**"], max_files: 5}
```

Issue #17 (quota-aware scheduling) is not implemented yet. Review and correction rounds use the same
pausing as everything else: an exhausted agent moves the task to `quota-paused`.

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
