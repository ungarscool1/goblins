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
| `GOBLINS_METRICS_LISTEN`    | `server.metrics_listen`    |
| `GOBLINS_CONSOLE_LISTEN`    | `console.listen`           |
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

## Operator console

`console` turns on the web console and its API, on a port of their own, with a token per
operator. See [console.md](console.md).

## Pull request descriptions

After each push, an agent reads the diff of the pushed commit and writes the description, read-only and
with no tools that can change anything. This costs one agent run per push. By default it is the
implementer's agent on its default model. To use something cheaper, set `stages.describer`, which has to
be on the implementer's runtime. If that run fails, the pull request still goes out, described from the
approved design instead.

## Usage limits and budgets

Goblins runs on subscriptions, so it works from what a CLI reports and never estimates what is left.

When an agent reports a usage limit, a rate limit or a lost login, the task pauses with its job, session and
branch intact, and everything else queued on that agent's account waits too. Other accounts keep
working. If the CLI said when the limit ends, the pause lasts until then. Otherwise it starts at
`policy.quota_pause`, `rate_limit_pause` or `auth_pause`, and doubles each time up to `max_limit_pause`.
After `max_limit_pauses` in a row the task blocks and asks a person. `/goblins resume` tries again
right away, including for the rest of the account.

| Setting | Default | Meaning |
| --- | --- | --- |
| `policy.quota_pause` | 30m | First pause after a usage limit |
| `policy.rate_limit_pause` | 5m | First pause after a rate limit |
| `policy.auth_pause` | 1h | First pause after a lost login |
| `policy.max_limit_pause` | 6h | Longest a pause grows to, when no reset time is known |
| `policy.max_limit_pauses` | 8 | Pauses in a row before blocking |
| `policy.max_changed_files` | none | Blocks a change touching more files than this, before it is checked or published |
| `stages.*.timeout` | per stage | Longest one run may take |
| `stages.*.fallback_model` | none | Continues the same session on this model when the configured one is limited. Planner, implementer and triager only |
| `stages.*.max_tool_calls` | none | Blocks a run making more tool calls than this. `/goblins resume` grants a new budget. Planner, implementer and triager only |

`stages.triager` judges review remarks on a model of its own, such as a cheaper one. It has to use the
implementer's agent and runtime. Without it, the implementer's settings apply.

Concurrency stays per account, with `max_concurrent` on an agent. Among jobs of equal priority, the
project running the fewest goes first. A project that does not name an agent never runs it. What each
run reports using is kept in its transcript.

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

## Validation

The `command` provider runs named checks with no credentials and, unless a check asks otherwise, no
network, against the exact commit. Checks live in the provider's `checks`, or in the repository with
`policy.validation_manifest`:

```yaml
version: 1
image: golang:1.27-alpine        # the default for checks below
checks:
  - {name: lint, stage: lint, run: "go vet ./..."}
  - name: test
    stage: test
    run: "go test -json ./... > report.json"
    timeout: 20m
    artifacts: [report.json]     # shown after the output, up to 4 KiB each
  - {name: vms, stage: integration, run: "./scripts/vm-suite", kvm: true, optional: true}
```

| Field | Meaning |
| --- | --- |
| `image` | Tools for this check. Use a fixed version: each report names the image it ran in |
| `timeout` | Longest this check may take. Defaults to the provider's `timeout` |
| `optional` | Reported, but does not stop a change or go back to the implementer |
| `network` | `none` or `egress` |
| `kvm` | Needs virtualization. A runtime without it refuses, and the task blocks saying so instead of the check failing |
| `artifacts` | Files the check leaves in the repository, up to 5 |
| `stage` | A label for the kind of check |

**What a change cannot do to its own checks.** A manifest is read from the base commit, so editing it
has no effect until a person merges that edit. In a manifest, `network: egress` and `kvm` are refused
unless the provider sets `manifest_network` or `manifest_kvm`. Set `policy.trusted_paths` to files the
checks must always take from the base commit, such as conformance tests:

```yaml
policy:
  validation_manifest: .goblins/validation.yaml
  trusted_paths: [conformance, Makefile, scripts/vm-suite]
```

Whatever a change does in those paths is replaced for the checks, and the status report names what it
edited. List everything the checks run. A trusted test suite run through an untrusted `Makefile` can
still be subverted. A real change to these files needs merging first.

With `manifest_only: true` on the provider, its `checks` can be empty, and a base commit with no
manifest blocks the task instead of checking nothing. Each check runs on its own, so a mock backend it
needs has to start inside its `run`. Separate service containers and staging environments with test
credentials are not supported yet.

## Skipping checks is an explicit choice

A project must name a `validation`, or set `policy.allow_unvalidated_draft_pr: true`. It must
name a `review` or list `stages.reviewers`, or set `policy.allow_skipped_review: true`. Without these, the configuration
is refused at startup, and a task that still reaches such a step blocks.

With the opt-in, the step reports `skipped`, never a pass: the commit is not recorded as
validated, and the status report says that nothing was tested. Use these only when a person
reviews every draft pull request.
