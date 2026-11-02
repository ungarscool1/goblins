# Configuration

Goblins reads one versioned YAML file (`--config FILE` or `GOBLINS_CONFIG`) or
the built-in mock profile (`--mock`). Loading is strict: unknown fields, unknown
provider types, dangling references, unsupported versions and inline secrets are
startup errors, and all problems are reported at once.

## Environment overrides

| Variable                    | Overrides                  |
| --------------------------- | -------------------------- |
| `GOBLINS_INSTALLATION_NAME` | `installation.name`        |
| `GOBLINS_DATA_DIR`          | `installation.data_dir`    |
| `GOBLINS_LISTEN`            | `server.listen`            |
| `GOBLINS_SHUTDOWN_TIMEOUT`  | `server.shutdown_timeout`  |
| `GOBLINS_LOG_LEVEL`         | `log.level`                |
| `GOBLINS_LOG_FORMAT`        | `log.format`               |
| `GOBLINS_STORAGE_DRIVER`    | `storage.driver`           |
| `GOBLINS_STORAGE_PATH`      | `storage.path`             |

## Secrets

An option whose name designates a secret (`token`, `secret`, `password`,
`private_key`, `api_key`, `credential`, `dsn`, ...) cannot hold a value. Use a
reference instead: `webhook_secret_env: MY_VAR` or `private_key_file: /run/secrets/key`.
`goblins config render` prints references as they are and masks everything else
that looks sensitive, including credentials embedded in URLs.

## Shape

Provider instances are declared per kind under a name of your choice, and
projects refer to them by name, so trackers, source control, agents and runtimes
can be combined freely. See [`configs/example.yaml`](../configs/example.yaml).

## Pull request descriptions

After each push, an agent reads the diff of the pushed commit and writes the description, read-only and
with no tools that can change anything. This costs one agent run per push. By default it is the
implementer's agent on its default model. To use something cheaper, set `stages.describer`, which has to
be on the implementer's runtime. If that run fails, the pull request still goes out, described from the
approved design instead.

## Skipping checks is an explicit choice

A project must name a `validation`, or set `policy.allow_unvalidated_draft_pr: true`. It must
name a `review` or list `stages.reviewers`, or set `policy.allow_skipped_review: true`. Without these, the configuration
is refused at startup, and a task that still reaches such a step blocks.

With the opt-in, the step reports `skipped`, never a pass: the commit is not recorded as
validated, and the status report says that nothing was tested. Use these only when a person
reviews every draft pull request.
