# Goblins

Goblins is a self-hosted, provider-agnostic orchestrator for autonomous software
engineering. It discovers work items, researches them, asks questions on the
ticket, obtains explicit design approval, then runs subscription-authenticated
coding agents in isolated workspaces and opens draft pull requests.

The roadmap lives in [issue #1](https://github.com/ungarscool1/goblins/issues/1).
Goblins never requires a model API key.

## Quick start (mock mode)

Mock mode needs no tracker credentials and no agent subscription login.

```bash
make build
```

```bash
./bin/goblins serve --mock
```

```bash
curl -s localhost:8080/readyz
```

Or in a container:

```bash
make image
```

```bash
docker run --rm -p 127.0.0.1:8080:8080 goblins:dev
```

## Development

| Command            | What it does                                         |
| ------------------ | ---------------------------------------------------- |
| `make test`        | Unit tests (no network, no tokens)                   |
| `make race`        | Unit tests with the race detector                    |
| `make integration` | Integration tests against the built binary           |
| `make lint`        | `go vet` and golangci-lint, pinned by `go run`       |
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

## Layout

| Path                     | Purpose                                                     |
| ------------------------ | ----------------------------------------------------------- |
| `cmd/goblins`            | CLI entry point                                             |
| `internal/core`          | Provider-neutral domain model, errors and contracts         |
| `internal/workflow`      | Deterministic workflow state machine                        |
| `internal/providers`     | Adapter registry and adapters                               |
| `internal/runtime`       | Execution helpers shared by runtimes                        |
| `internal/storage`       | Persistence                                                 |
| `internal/api`           | HTTP surface: health, readiness                             |
| `internal/observability` | Structured logs with secret redaction                       |
| `internal/app`           | Composition root                                            |
| `internal/archtest`      | Enforces provider neutrality and dependency rules           |
| `docs/adr`               | Architecture decision records                               |

`internal/archtest` fails the build if a core package mentions a concrete product
or if an adapter imports the workflow engine or storage.

## License

Apache-2.0, see [LICENSE](LICENSE).
