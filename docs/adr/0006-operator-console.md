# ADR 0006: Operators log in to the console with configured tokens

**Status:** accepted

**Context.** Issue #20 asks for a web console and an API, to watch tasks, read their conversations
and transcripts, answer questions and act on tasks without a shell on the host. They show what agents
read and wrote, and their actions approve designs. So they need a login, and Goblins has no users of
its own. The options were:

1. **Sign in with the tracker**, such as a GitHub OAuth App. It reuses the permissions people already
   have, but needs an OAuth application per installation, a callback reachable from the browser, and
   a mapping from tracker access to console roles that differs for every tracker. A work installation
   on a tracker without OAuth for its users, such as Jira with no admin access, could not use it.
2. **A reverse proxy in front**, with the console trusting a header naming the user. Strong if set up
   right, but the console cannot tell when it is not, and a direct connection forges the header.
3. **Tokens in the configuration**, one per operator, given by reference like every other secret.

**Decision.** Option 3, with the console on a port of its own that is off by default.

- `console.operators` lists each operator with a name, a role and a `token_env` or `token_file`. A
  token shorter than 32 characters stops startup. Tokens are compared in constant time.
- Roles are `viewer`, who reads everything, and `operator`, who can also act. `console.read_only`
  refuses every action whatever the role, for a work profile where only watching is allowed.
- A browser logs in once with its token and gets a session cookie: random, in memory only,
  `HttpOnly`, `SameSite=Strict`, always `Secure`, and lasting `session_ttl`
  (12 hours by default). A restart logs everyone out. Every form also carries the session's own
  token, and requests a browser marks as coming from another site are refused.
- The JSON API takes the token as a bearer token and nothing else, so a browser cannot be made to
  call it.
- Ten failed logins in a minute make logins wait for the rest of that minute.
- Pages allow no script at all, cannot be framed and are not cached. Everything is escaped, nothing
  is rendered as Markdown, and transcripts were redacted before they were stored.
- **The console never changes a task itself.** Each action becomes an operator comment, the same
  event `goblins comment` enters, carrying the operator's name. The workflow applies it with its usual
  rules, the event log keeps it, and the task's history says who did what. An approval names the
  revision and hash on screen, so a newer design is never approved by mistake.
- Nothing in the console reads or shows configuration, credentials, agent logins or a shell.

**Consequences.** Adding or removing an operator means changing configuration and restarting. Tokens
are long-lived secrets, so they belong in a password manager and should be rotated like any other.
There is no single sign-on, and no per-project roles: an operator acts on every project of the
installation. Both can be added later, behind a proxy that authenticates (option 2), without changing
how actions reach the workflow. An operator answers as themselves, but the tracker never sees that
answer: it goes to the planner and the console only.
