# ADR 0004: Credential-bearing agents never execute repository code

**Status:** accepted

**Context.** A coding agent needs subscription credentials, and repositories
contain untrusted code. A build script, test or hook that runs next to the
credentials can read them, which would hand a subscription login to anyone who can
get code into a repository. Network filtering and file permissions inside one
container do not solve this, since the agent process itself must read them.

**Decision.** Split the roles into separate workloads:

| Workload          | Credentials        | Runs repo code | Network         |
| ----------------- | ------------------ | -------------- | --------------- |
| Agent             | Agent subscription | No             | Egress          |
| Validation        | None               | Yes            | None by default |
| Workspace prepare | SCM, short-lived   | No             | Egress          |
| Commit and bundle | None               | No             | None            |
| Publish (push)    | SCM, short-lived   | No             | Egress          |

- `core.ToolPolicy` has no way to grant command execution. Agent adapters must enforce
  this with the CLI's own restrictions, and fail closed if those are unavailable.
- Tests run on a separate scratch copy of one commit. Repository code therefore cannot
  alter the workspace the agent reads and writes next, so it cannot plant symlinks, hooks
  or configuration there. Results return to the agent only as data, in a bounded loop.
- Checkouts use `core.symlinks=false`, so a repository cannot plant a symlink that leads
  the agent's file tools from the workspace to its credentials.
- A token never coexists with a repository untrusted content could have touched. Prepare
  wipes the directory first. Publish reads only a bundle file into a fresh repository,
  verifies the recorded commit hash, and pushes from there. Hooks and Git configuration
  written into the workspace are never honored while a token exists.

**Consequences.** The agent cannot iterate interactively with a shell, so each
test cycle costs one workflow loop. This is a deliberate trade: slower feedback in
exchange for a boundary that does not depend on the model behaving.

**Known limitations.** Agent egress is not filtered per destination in P0, so a
prompt-injected agent could send out what it can read, which is the workspace. Treat
workspace contents as readable by anyone who can influence the prompt, and do not put
private repositories under an agent that reads untrusted issues without considering this.

The credentials themselves depend on the CLI's restricted mode confining file tools to
the working directories: that is enforced by the CLI, not by the kernel. Those credentials
sit in the same container as the process using them. A credential gateway, so that the
agent's container holds no long-lived credential at all, is tracked in #16.
