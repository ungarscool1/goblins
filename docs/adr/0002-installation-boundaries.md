# ADR 0002: One installation, one set of credentials

**Status:** accepted

**Context.** The same person may run Goblins at home and at work, with a
different agent subscription account, tracker and repositories.

**Decision.** An *installation* is one deployment with its own configuration,
data directory, database and agent login. Nothing is shared between
installations and no mechanism for doing so exists. Agent CLI subscription
authentication is local to the installation that performed the login.

**Assumptions stated explicitly.**

- Agent CLIs authenticate through a subscription login made by a person on that
  installation. Goblins never requires, reads or falls back to model API keys.
- Re-authentication can require a person. Goblins pauses the task and asks; it
  cannot perform an unattended login.
- Usage is not unlimited. Quota exhaustion pauses work and never triggers paid
  fallback.

**Consequences.** Secrets are never stored in configuration files: options that
name a secret must use `*_env` or `*_file` indirection, and the loader rejects
inline values. This is what makes `goblins config render` safe to share.
