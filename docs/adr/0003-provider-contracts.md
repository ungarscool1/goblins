# ADR 0003: Compiled-in adapters behind capability-negotiated contracts

**Status:** accepted

**Context.** Trackers, SCMs, agents and runtimes must be interchangeable, and new
ones must not require touching workflow logic. Features differ widely: a tracker
may have no write access, a runtime may have no volumes.

**Decision.**

- One contract per concern. Tracker and SCM are separate even when one product
  provides both.
- Optional features are named capabilities reported by `Info()`. Missing features
  yield `KindUnsupported`, which the workflow surfaces as a visible state.
- A registry maps `(kind, type)` to Go factories, with a contract version check.
- A single contract test suite per kind is the definition of correct behavior.
- `AgentProvider.Run` is one blocking call with `ctx` for cancellation and a sink
  for streamed events, and `ResumeSessionID` for resuming. This covers launch,
  resume, cancel and streaming without a handle type that would need its own
  crash-recovery semantics: after a crash the orchestrator cleans up the old
  workload and resumes from the persisted session ID.
- `ToolPolicy` is abstract and has no way to grant command execution. See ADR 0004.

**Consequences.** Adding a provider is local. Fakes are real implementations held
to the same suite, so tests exercise behavior rather than mocks of it.
