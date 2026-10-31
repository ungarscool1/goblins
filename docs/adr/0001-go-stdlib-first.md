# ADR 0001: Go, standard library first, single binary

**Status:** accepted

**Context.** Goblins is a long-running control plane deployed by individuals on
Docker Compose or Kubernetes. It orchestrates untrusted-code workers, so its own
dependency surface should be small and auditable.

**Decision.** Write the service in Go, as one statically linked binary without
cgo, using the standard library wherever practical (`net/http`, `log/slog`,
`database/sql`, `crypto`). Add third-party modules only for what would be
unreasonable to write: YAML parsing and SQL drivers. No web or DI framework.

**Consequences.** Images stay minimal (distroless, non-root). Adapters talk to
external HTTP APIs through small purpose-built clients rather than vendor SDKs,
which means maintaining those clients but keeps provider code readable and easy
to fake.
