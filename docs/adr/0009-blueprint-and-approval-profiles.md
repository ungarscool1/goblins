# ADR 0009: One blueprint in code, approval profiles in configuration

**Status:** accepted

**Context.** Issue #60 asks for blueprints mixing deterministic steps with agent reasoning, and
for low-risk work to run without routine approvals while sensitive work stops for a person. A
general, user-defined graph of typed nodes would let a project, or a repository, rearrange gates:
the safety of Goblins rests on the order "approved design, then implementation, then checks on
the exact commit, then a draft". Templates and task graphs are #25.

**Decision.**

- The blueprint is the state machine (`workflow.Decide`), versioned as `issue-to-draft@1`, with
  typed stages: agent, approval, command checks, environment actions, review, publish. Projects
  configure what runs in a stage (agents, checks, environment steps, reviewers, bounds), never the
  order or whether a gate applies. Each agent run records the blueprint and what executed it.
- Approval is a **profile**. `strict` is the default. `unattended` is an operator's explicit
  choice: a person with approval permission invokes it per item with a label, the design is still
  posted and only then approved, recorded with the profile's hash and that person. Risk is decided
  from the commit's changed paths against the profile's scope and built-in sensitive paths, never
  from a model's assessment. Leaving the scope withdraws the approval and waits for a person.
  The transition table is unchanged except that a change can go back to `design-proposed`:
  nothing reaches a worker without passing it.
- Tools beyond file tools are allow-listed per stage, from what the agent's image contains.

**Consequences.**

- A low-risk scope can go from label to draft pull request with nobody asked, and a change
  touching authentication, infrastructure or automation cannot, whatever the ticket says.
- Changed paths are the only risk signal. A change inside the allowed paths can still be wrong;
  checks, environment verification, review and the draft are what catch that.
- New blueprints mean new code, reviewed like any other. A configurable graph, if #25 needs one,
  must keep these gates as fixed nodes.
