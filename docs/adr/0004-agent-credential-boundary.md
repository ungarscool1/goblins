# ADR 0004: Credential-bearing agents never execute repository code

**Status:** accepted

**Context.** A coding agent needs subscription credentials, and repositories
contain untrusted code. A build script, test or hook that runs next to the
credentials can read them, which would hand a subscription login to anyone who can
get code into a repository. Network filtering and file permissions inside one
container do not solve this, since the agent process itself must read them.

**Decision.** Split the two roles into separate workloads on one workspace volume:

| Workload              | Credentials        | Runs repo code | Network         |
| --------------------- | ------------------ | -------------- | --------------- |
| Agent                 | Agent subscription | No             | Egress          |
| Validation            | None               | Yes            | None by default |
| Workspace prepare     | SCM, short-lived   | No             | Egress          |
| Publish (push)        | SCM, short-lived   | No             | Egress          |

- `core.ToolPolicy` has no way to grant command execution. Agent adapters must
  enforce it with the CLI's own restrictions and fail closed if unavailable.
- Tests run in a credential-free workload, and the workflow feeds their output back
  to the agent as data, in a bounded loop.
- Publishing fetches commits out of the untrusted workspace into a fresh repository
  with trusted configuration, then pushes from there. Hooks and Git configuration
  written by the agent or repository are never honored while a token is present.

**Consequences.** The agent cannot iterate interactively with a shell, so each
test cycle costs one workflow loop. This is a deliberate trade: slower feedback in
exchange for a boundary that does not depend on the model behaving.

**Known limitation.** Agent egress is not filtered per destination in P0, and a
prompt-injected agent could write content it can read into the workspace. The agent
cannot read outside its working directories, so credentials are out of reach by
that route, but workspace contents should be considered public to anyone who can
influence the prompt. Egress control and a credential gateway are tracked in #16.
