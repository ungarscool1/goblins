# Threat model

Goblins runs agents on untrusted input while holding valuable credentials. This page says what
it protects, how, and what it does not.

## What is worth protecting

| Asset                    | Where it lives                     | Impact if taken                    |
| ------------------------ | ---------------------------------- | ---------------------------------- |
| Agent subscription login | Agent config volume, or env token  | Use of your account and its usage  |
| GitHub App private key   | Orchestrator only                  | Access to all installed repositories |
| Installation tokens      | Orchestrator; prepare and publish workloads | One repository, about an hour |
| Webhook secret           | Orchestrator                       | Forged events; still subject to permission checks |
| Docker API               | Orchestrator, through a proxy      | Root on the worker host            |
| Merge to default branch  | Nobody: Goblins never merges       |                                    |

## Who can attack

- **Anyone who can write on a ticket**, on public repositories: anyone. Comments may contain prompt
  injection.
- **Anyone who can get content into a repository**, including dependencies. Code, tests and
  files may be hostile.
- **The model itself**, once prompt-injected.
- **The network**, for forged or replayed webhooks.

Out of scope: someone with shell or Docker access to the host, who already controls everything; and
GitHub, Anthropic or Docker being compromised.

## Controls

| Threat                                | Control                                                      | Enforced by |
| ------------------------------------- | ------------------------------------------------------------ | ----------- |
| Model or comment approves a design    | Only a new comment by a permitted human, naming the exact revision. Rechecked before implementing and publishing | Go code     |
| Stranger spends your subscription     | Comments below `min_input_permission` trigger nothing; intake is permission-gated | Go code |
| Comment injection reaches the model   | Those same comments are withheld from prompts                | Go code     |
| Repository code reads agent login     | Agents have no command-running tools. Tests run elsewhere, without credentials | CLI restricted mode, separate containers |
| Agent file tools read the login       | File tools confined to the working directory; repository symlinks are not checked out | CLI restricted mode; Git config |
| Tests poison the agent's workspace    | Tests run on a scratch copy and never see the workspace      | Separate volumes |
| Hostile `.git` steals a token         | A token never coexists with a repository content could have touched | Workflow of workloads |
| Tampering between validation and push | The pushed commit must equal the validated hash              | Go code, Git |
| Container escape to the host          | Non-root, no capabilities, no new privileges, no host mounts, resource limits | Docker |
| Docker socket in a worker             | Only named volumes can be expressed                          | Go code     |
| Forged or replayed webhook            | HMAC checked first, repository allow-list, idempotent event identity | Go code |
| Comment loop with itself              | Automation's comments are ignored                            | Go code     |
| Paid API fallback                     | Billing variables unset, API key options refused, API-keyed sessions discarded | Go code, wrapper |
| Runaway cost                          | Concurrency limits, bounded loops, deadlines, pause on quota | Go code |
| Secrets in logs                       | `core.Secret`, redacting logger and transcript, no raw CLI output in messages | Go code |
| Secrets in check output on the ticket | Validation gets no credentials; output is redacted, bounded and fenced. Redaction only knows common shapes, so a secret a repository already holds can still be printed | Go code, separate containers |
| Agent pushes to a protected branch    | Only `goblins/*`, never forced; App lacks workflow permission | Go code, GitHub |

## Known gaps

These are real and accepted for P0. Read them before pointing Goblins at anything sensitive.

1. **The login is in the same container as the agent process.** The boundary around it is the CLI's
   restricted mode, which is enforced by the CLI, not the kernel. A bug there, or in the agent, could
   expose it. Moving it to a credential gateway is #16.
2. **Agents have unfiltered egress.** A prompt-injected agent could send out what it can read: the
   workspace. Do not run an agent that reads untrusted tickets on a private repository whose contents
   must stay secret. Per-destination egress control is #16.
3. **Workspace contents can reach the pull request.** An injected agent could write anything into the
   code. That is why every change is a draft for a person to review, and why nothing merges.
4. **Prompt injection is reduced, not solved.** Delimiters and system prompts help; the hard boundaries
   above are what limit the damage.
5. **`goblins comment` trusts shell access.** Whoever has a shell can approve under any name.
6. **Tests with network access** can reach anything the bridge routes to, including your LAN, if you
   set `network: egress`. They still have no credentials.
7. **Restricted mode is only partly verified.** One real run, on CLI 2.1.310 on macOS rather than the
   agent image, confirmed the flags are accepted, that only the allowed tools are offered, and the
   stream format. Whether it stops `Read` reaching the login files, and session resume across
   containers, are untested. Check both with a throwaway login, per [claude.md](claude.md).
8. **Named volumes have no size limit.** A hostile repository could fill the disk.
9. **A compromised orchestrator controls its Docker engine.** The proxy filters API paths, not what
   a created container asks for, so it could start a privileged container or mount host paths. Run
   on a dedicated, disposable host or VM, and do not share the engine with anything important.
   A policy-enforcing boundary is #16.
