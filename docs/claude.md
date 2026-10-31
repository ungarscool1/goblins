# Claude Code adapter

The `claude-cli` agent runs the Claude Code CLI under a **subscription login** in an
isolated workload. It never reads, accepts or falls back to a model API key.

## Logging in

Subscription logins belong to one installation and are never shared between
installations. Keep work and personal accounts in separate Compose projects.

Either log in interactively; the result lives in the installation's config volume:

```bash
docker compose run --rm -it agent-login
```

Or create a long-lived token with `claude setup-token` and pass it by reference:

```yaml
agents:
  claude:
    type: claude-cli
    options:
      image: goblins-agent-claude:dev
      oauth_token_env: CLAUDE_CODE_OAUTH_TOKEN
```

On macOS the host's login is held in the Keychain, so it cannot be mounted into a
container. Use one of the two options above.

Goblins cannot log in unattended. When a login expires, tasks move to `quota-paused`,
an operator is notified, the ticket says so, and work resumes after logging in again.

## Configuration

| Option             | Meaning                                                        |
| ------------------ | -------------------------------------------------------------- |
| `image`            | Required. Agent image; see `deploy/images/agent-claude`        |
| `config_volume`    | Volume with the login and session data; give each installation its own |
| `oauth_token_env`  | Optional subscription token from `claude setup-token`          |
| `memory_mb`, `cpus`, `pids` | Resource limits                                       |

Choose model and effort per stage in the project, using an alias the installed CLI
understands. Nothing is hardcoded, and no fallback model is configured:

```yaml
stages:
  implementer: {agent: claude, runtime: docker, model: opus, effort: high}
```

Verified against CLI 2.1.310 and 2.1.316: `--effort` accepts `low`, `medium`, `high`,
`xhigh` and `max`, and `--model` accepts aliases such as `opus` or a full model name.

## How a run is restricted

Each run uses:

```text
claude -p --output-format stream-json --verbose --restricted --strict-mcp-config
  --disable-slash-commands --tools <list> --allowedTools <list> --permission-mode dontAsk
```

- `--restricted` removes tools that run commands or code, ignores user, project and
  local settings files (so repository hooks do not load), and confines file tools to the
  working directories. Credentials in the config volume are outside those directories.
- `--strict-mcp-config` ignores MCP servers configured by the repository.
- The tool list derives from the role's policy only: `Read, Glob, Grep`, plus `Edit,
  Write` for implementers, plus `WebFetch, WebSearch` where research is allowed. There is
  no way to add a command-running tool. See [ADR 0004](adr/0004-agent-credential-boundary.md).
- Planners get the workspace mounted read-only.

The first run on each image checks `claude --help` for these options. If any is
missing, the adapter refuses to run and the task becomes `blocked`.

### Not billing an API account

- The wrapper unsets `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_BASE_URL`,
  and the Bedrock, Vertex and Foundry switches before starting the CLI, whatever the image
  sets. Workloads inherit nothing from the orchestrator's environment.
- The configuration refuses `api_key*` options.
- If the CLI's init event reports an API key source, the run is discarded and the task
  pauses for a person.
- `AuthStatus` calls `claude auth status`, which costs no usage, and reports anything other
  than a first-party subscription login as unusable.

## Session durability

- The session ID is checkpointed as soon as it appears, so a crash can resume.
- Sessions are stored in the config volume, keyed by working directory, and the working
  directory is fixed, so resume works across containers.
- Losing the config volume loses resumability and the login. The next run starts a new
  session from the durable design and ticket conversation; no approval is lost.
- If the CLI no longer knows a session, the job starts a new one.
- Quota errors keep the session for resuming after the pause.

## Tests

`internal/providers/claude/testdata/claude` is a fake executable with the same flags and
stream format, so CI uses no tokens. To check the real CLI's stream format and restricted
mode, run the opt-in smoke test. **It spends a small amount of subscription usage.**

```bash
GOBLINS_SMOKE_CLAUDE=1 go test -tags smoke -run ClaudeSmoke -v ./test/integration/
```
