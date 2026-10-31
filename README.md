# Goblins

Goblins is a self-hosted, provider-agnostic orchestrator for autonomous software
engineering. It discovers work items, researches them, asks questions on the
ticket, obtains explicit design approval, then runs subscription-authenticated
coding agents in isolated workspaces and opens draft pull requests.

The roadmap lives in [issue #1](https://github.com/ungarscool1/goblins/issues/1).
Goblins never requires a model API key.

## Status

The P0 slice works end to end in tests: new GitHub issue, design conversation, explicit approval,
implementation under a subscription-authenticated Claude CLI, isolated tests, draft pull request and
a status report. It has been verified offline with a fake GitHub and a fake CLI, on real Git and real
Docker containers. **It has not yet been run against real GitHub or a real model.**
[test/smoke/real.sh](test/smoke/real.sh) does that, on a disposable repository you provide.

Not in P0: Codex, Kubernetes, Jira, a review loop, egress filtering, a dashboard, merging. See the
[roadmap](https://github.com/ungarscool1/goblins/issues/1) and the known gaps in the
[threat model](docs/threat-model.md).

## Quick start

Mock mode needs Docker Compose and nothing else. The agent and tracker are fakes and no model is
involved:

```bash
cd deploy/compose && docker compose up -d --build
```

```bash
curl -s localhost:8080/mock/demo/issues -d '{"title":"Add a thing","body":"Please"}'
```

```bash
docker compose exec goblins goblins status --mock
```

```bash
curl -s localhost:8080/mock/demo/issues/1/comments -d '{"body":"/approve-design 1"}'
```

[docs/quickstart.md](docs/quickstart.md) continues from there to a real installation with a
GitHub App and a Claude subscription login. Read the [threat model](docs/threat-model.md) first,
including its known gaps.

Without Docker:

```bash
make build && ./bin/goblins serve --mock
```

## Development

| Command            | What it does                                         |
| ------------------ | ---------------------------------------------------- |
| `make test`        | Unit tests (no network, no tokens)                   |
| `make race`        | Unit tests with the race detector                    |
| `make integration` | Integration tests against the built binary           |
| `make docker-test` | Tests and the acceptance scenario on real containers |
| `make lint`        | `go vet` and golangci-lint, pinned by `go run`       |
| `make compose-smoke` | Starts the Compose distribution and walks through it |
| `make image`       | Builds the container image                           |

CI runs the same commands, needs no repository secrets and consumes no LLM tokens.

## Configuration

See [docs/configuration.md](docs/configuration.md). In short: a versioned YAML
file plus `GOBLINS_*` environment overrides, strictly validated at startup.
Secrets are passed by reference, never inline.

```bash
./bin/goblins config check --config configs/example.yaml
```

```bash
./bin/goblins config render --config configs/example.yaml
```

Adapters and how to add one are described in [docs/providers.md](docs/providers.md).
The state machine, its invariants and comment commands are in [docs/workflow.md](docs/workflow.md).

## Layout

| Path                     | Purpose                                                     |
| ------------------------ | ----------------------------------------------------------- |
| `cmd/goblins`            | CLI entry point                                             |
| `internal/core`          | Provider-neutral domain model, errors and contracts         |
| `internal/workflow`      | Deterministic workflow state machine                        |
| `internal/scheduler`     | Inbox, polling, job dispatch and orphan cleanup             |
| `internal/pipeline`      | Job handlers connecting the workflow to providers           |
| `internal/providers`     | Adapter registry and adapters                               |
| `internal/runtime`       | Docker runtime, workspace scripts and shared helpers        |
| `internal/storage`       | Persistence                                                 |
| `internal/api`           | HTTP surface: health, readiness                             |
| `internal/observability` | Structured logs with secret redaction                       |
| `internal/app`           | Composition root                                            |
| `internal/archtest`      | Enforces provider neutrality and dependency rules           |
| `docs/adr`               | Architecture decision records                               |
| `test/e2e`               | Acceptance test, the MVP release gate                       |
| `test/smoke`             | Opt-in test against real services                           |
| `examples/hello`         | A tiny project to try on a disposable repository            |

`internal/archtest` fails the build if a core package mentions a concrete product
or if an adapter imports the workflow engine or storage.

## Documentation

| Page                                      | Topic                                  |
| ----------------------------------------- | -------------------------------------- |
| [Quick start](docs/quickstart.md)         | Mock mode, real setup, operations      |
| [Walkthrough](docs/walkthrough.md)        | One real issue; what to do on failures |
| [Architecture](docs/architecture.md)      | Components, rules, how it is tested    |
| [Threat model](docs/threat-model.md)      | Controls and known gaps                |
| [Workflow](docs/workflow.md)              | States, invariants, commands           |
| [Providers](docs/providers.md)            | Contracts and how to add an adapter    |
| [Configuration](docs/configuration.md)    | File format and environment            |
| [GitHub](docs/github.md)                  | App registration and permissions       |
| [Claude](docs/claude.md)                  | Login, restrictions, durability        |
| [Docker runtime](docs/docker-runtime.md)  | Worker confinement and workspaces      |
| [Decisions](docs/adr)                     | Architecture decision records          |

## License

Apache-2.0, see [LICENSE](LICENSE).
