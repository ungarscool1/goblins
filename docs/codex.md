# Codex CLI adapter

The `codex-cli` agent runs the Codex CLI under a **ChatGPT subscription login** in an
isolated workload. It never reads, accepts or falls back to a model API key. It is optional:
a project that does not name it never builds or runs it, so an installation with only Claude
Code has nothing to configure and nothing to fail.

## Logging in

Logins belong to one installation. Device login works without a browser on the host:

```bash
docker compose --profile codex run --rm -it codex-login
```

The login lives in the installation's `GOBLINS_CODEX_VOLUME`. Keep work and personal accounts
in separate Compose projects. When a login expires or the usage limit is reached, a task using
Codex as its only stage pauses and an operator is told; as a reviewer, the next reviewer takes
over (see below).

## Configuration

```yaml
agents:
  codex:
    type: codex-cli
    max_concurrent: 1
    options:
      image: goblins-agent-codex:dev
      home_volume: goblins-personal-codex-home
projects:
  - id: app
    stages:
      planner: {agent: claude, runtime: docker}
      implementer: {agent: claude, runtime: docker, model: opus, effort: high}
      reviewers:
        - {agent: codex, runtime: docker, model: gpt-5-codex, effort: high}
        - {agent: claude, runtime: docker}
```

| Option                      | Meaning                                                  |
| --------------------------- | -------------------------------------------------------- |
| `image`                     | Required. Agent image; see `deploy/images/agent-codex`   |
| `home_volume`               | Volume with the login and sessions, one per installation |
| `memory_mb`, `cpus`, `pids` | Resource limits                                          |

`api_key*` and `access_token*` options are refused. Models and efforts are read from the CLI's
own catalog (`codex debug models --bundled`), which uses no quota: an effort that the catalog
says a model cannot do blocks the task with an explanation, before anything is spent. A model
that the catalog does not list is passed on, as catalogs lag behind.

## How a run is restricted

[ADR 0004](adr/0004-agent-credential-boundary.md) requires that an agent holding credentials
runs no commands. A sandbox does not achieve this for Codex: `read-only` still lets a command
read the login next to it. So:

- `--disable shell_tool --disable unified_exec`, plus every other feature the CLI reports as
  enabled that is not on a short list known to add no tools. A tool in a future CLI starts off.
- `--ignore-user-config --ignore-rules`, `mcp_servers={}`, `project_doc_max_bytes=0`,
  `approval_policy="never"`: nothing from the repository or the volume adds instructions or tools.
- `sandbox_mode` comes from the abstract policy, and planners and reviewers get their workspace
  mounted read-only. There is no way to configure anything else.
- The stream is checked: a run that shows a command or tool call anyway is discarded, and the
  task is blocked. A CLI without these switches is refused before it runs.

### Not billing an API account

- `OPENAI_API_KEY`, `CODEX_API_KEY`, `CODEX_ACCESS_TOKEN`, `OPENAI_BASE_URL` and the like are
  unset in the workload, whatever the image sets, and `forced_login_method="chatgpt"` is passed.
- The stream does not say what a run bills to, so `codex login status` runs before **every** run,
  and anything other than a ChatGPT login stops it with instructions.

## Review

Any agent can review: `stages.reviewers` lists them, and `workflow.ReviewPromptVersion` pins what
they are asked. See [Review](workflow.md#review) for the review stage and its fallback chain.

A reviewer gets a read-only checkout of one exact commit, the diff against the base as data, and
a schema with severity and confidence. Its output is parsed strictly, and it is never taken as a
pass if it cannot be read. Reviews are published as comments: an automated reviewer never approves
or requests changes on the SCM.

### Native review on GitHub

Where a reviewer lives on GitHub itself, use it first, with the local CLI as the fallback:

```yaml
reviews:
  native:
    type: github-native-review
    options:
      app_id: 123456
      private_key_file: /run/secrets/github-app.pem
      reviewer_login: chatgpt-codex-connector[bot]   # as it appears on its reviews
      trigger_comment: "@codex review"
      verified: true   # only after you have seen it answer this App's comment
      wait_seconds: 600
```

A mention is not a promise. Goblins cannot know whether that reviewer responds to a comment from
another App, so this does nothing until an operator sets `verified`. Only a review by that login on
the exact commit counts, and one that does not arrive in time means unavailable, not approved.
The request is made once per commit. This path has been tested against a fake GitHub only.

## Tests

`internal/providers/codex/testdata/codex` is a fake executable with the same subcommands and
stream format, so CI uses no tokens and needs no login.

## Not verified with a real login

Everything above runs against the fake CLI in CI. Flag names, feature names and the catalog format
were checked against `codex-cli 0.164.0`, without a login and without calling a model. The stream
format comes from the CLI's documented `--json` output. These need the opt-in smoke test, which
**spends a small amount of subscription usage**, before Codex is relied on:

- that a run with `shell_tool` and `unified_exec` off still works, and what it can read;
- whether `workspace-write` file edits work inside the agent container, as Codex applies its
  sandbox with kernel features the container may deny;
- the exact text of `codex login status` and of quota and login errors.

```bash
GOBLINS_SMOKE_CODEX=1 go test -tags smoke -run CodexSmoke -v ./test/integration/
```

With its shell off, Codex has fewer ways to read a repository than Claude Code's file tools give.
The diff in the prompt is what makes review work regardless. Prefer Claude Code as implementer
until the smoke test says otherwise.
