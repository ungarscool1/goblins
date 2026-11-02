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

## Commit messages and pull request titles

Commits and pull request titles follow [Conventional Commits](https://www.conventionalcommits.org).
The implementer words the subject of each commit, so a correction round says what it corrected, and
the description's agent words the pull request's title from the whole diff, which matters when a pull
request is squashed. An answer that does not fit the form falls back to the ticket's title as a `chore`,
since nothing says it is a feature or a fix. So does a project with no agent to describe the diff.
A change that breaks what existing users rely on is marked with `!`, as in `feat!: drop v1`.

A pull request keeps the title it was opened with, including one a person edited, however many pushes
follow. The only exception is a `chore` title from that fallback, which is replaced as soon as a
description run words a real one.

To commit under the ticket's title as it is, set `policy.commit_style: plain`. Either way, a subject
cannot carry a directive like `[skip ci]`, a mention or a closing reference to another ticket.

## Skipping checks is an explicit choice

A project must name a `validation`, or set `policy.allow_unvalidated_draft_pr: true`. It must
name a `review` or list `stages.reviewers`, or set `policy.allow_skipped_review: true`. Without these, the configuration
is refused at startup, and a task that still reaches such a step blocks.

With the opt-in, the step reports `skipped`, never a pass: the commit is not recorded as
validated, and the status report says that nothing was tested. Use these only when a person
reviews every draft pull request.
