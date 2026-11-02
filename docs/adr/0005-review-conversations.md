# ADR 0005: Review feedback is judged, discussed and never cleared by Goblins

**Status:** accepted

**Context.** Copying a change request to an agent and pushing a fix treats reviewers as a task queue.
It changes correct code when a reviewer is mistaken, cannot ask what was meant, and invites a reviewer's
words, which anyone with access can write, to steer an agent holding credentials.

**Decision.**

- Signals only prompt a read of the SCM's current state. Events are never trusted for what they say
  about a pull request, which makes replays and polling harmless.
- A finding's state lives in the task, committed atomically with the transition it causes. It is not
  in an agent session, so losing one loses nothing.
- A model judges, among a fixed set of outcomes. Go code decides what those outcomes can do: none
  can approve, resolve, widen the design or post outside the finding's own conversation.
- A push proves nothing. Findings stay unresolved until the SCM says otherwise, and Goblins has no code
  path that dismisses a review or resolves a thread.
- Conversations are bounded and escalate to a person, and only a person's words reopen a finding.

**Consequences.** Each new remark costs one read-only agent run, on top of any correction. A reviewer
who never resolves threads keeps the gate at `waiting`, which is correct: the gate reports, and people
decide. Optional, capability-gated thread resolution by Goblins is deliberately not implemented, as
nothing needs it yet.
