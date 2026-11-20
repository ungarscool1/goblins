# Development environments

Unit tests in a container cannot show that an infrastructure change works. A project can name a
**development environment**: after a commit passes its checks, Goblins deploys it there, verifies
it, and feeds a failure back to the implementer, within the project's repair rounds. Only a commit
that passed its checks is deployed, and only a verified one is published.

Nothing about any infrastructure is built in. An environment provider says what the steps are; Goblins
leases targets, runs the steps in order, keeps track of what ran, and decides what a failure means.

## Configuration

```yaml
environments:
  dev:
    type: command
    options:
      image: alpine/k8s:1.33.4                # whatever your steps need
      targets: [dev-1, dev-2]                 # shared targets, one task at a time; omit for one per task
      network: egress                         # through the egress proxy, if you run one
      test_secrets:                           # dev-only credentials, see below
        - {name: KUBECONFIG_DATA, env: GOBLINS_DEV_KUBECONFIG}
      allow_destructive: true                 # for the reset step below
      steps:
        - {name: plan, phase: deploy, kind: read, run: "terraform plan -input=false"}
        - {name: apply, phase: deploy, kind: mutate, idempotent: true,
           run: "helm upgrade --install app ./chart -n app-$GOBLINS_ENV_TARGET --wait"}
        - {name: ready, phase: verify, kind: read, run: "kubectl -n app-$GOBLINS_ENV_TARGET rollout status deploy/app"}
        - {name: smoke, phase: verify, kind: read, run: "./scripts/smoke $GOBLINS_ENV_TARGET"}
        - {name: events, phase: evidence, kind: read, run: "kubectl -n app-$GOBLINS_ENV_TARGET get events"}
        - {name: reset, phase: release, kind: destructive, idempotent: true,
           run: "helm uninstall app -n app-$GOBLINS_ENV_TARGET"}
projects:
  - id: app
    validation: checks
    environment: {name: dev, lease: 2h}
```

| Phase | When | Kinds allowed |
| --- | --- | --- |
| `acquire` | Once per lease, such as provisioning a target | read, mutate |
| `deploy` | For each commit that passed its checks | read, mutate |
| `verify` | After deploying: readiness, smoke, integration | read |
| `evidence` | After a failure: logs, events, diagnostics, added to the failure | read |
| `release` | When the commit is verified, or the task ends | read, mutate, destructive (with `allow_destructive`) |

Steps see the tested commit read-only in their working directory, and `GOBLINS_ENV`,
`GOBLINS_ENV_TARGET`, `GOBLINS_ENV_EPHEMERAL`, `GOBLINS_SHA`, `GOBLINS_STEP` and `GOBLINS_TASK`.
Their output is evidence: each step is a line in the status report and the task's transcript,
with the commit it ran for, and every step is logged as `environment operation` with the task,
target, step, kind and result.

## Targets and leases

- **Without `targets`,** each task gets its own environment, named from the task
  (`goblins-<hash>`), provisioned by the `acquire` steps and destroyed by the `release` steps.
- **With `targets`,** a task leases one, exclusively. A task that finds them all leased waits,
  without using up its attempts, and its job says which tasks hold them. A failed check keeps the
  target for the repair round; a verified commit, or the end of the task, gives it back.
- Leases are kept in the database, so they survive restarts. A lease not used for `lease` (two
  hours by default) can be taken over, and a target still held by a task that has finished, or
  that no longer exists, is taken back.
- **A target is reset before it is reused.** Before its first change, a task records that the
  target needs a reset, apart from the lease, and only a successful release clears it. Whoever
  holds the target next, however they came to, runs its `release` steps first if that record
  names another task. A release cut short by a transient error is tried again, and the task's
  workspace is kept for it.
- **When a reset fails,** the target stays reserved, marked broken, so no other task deploys onto
  it in an unknown state. A person restores it, then:

```bash
goblins environments                          # leases, holders and notes
goblins environments --release env:dev:dev-1  # give a restored target back
```

## Interruptions

A step marked `idempotent` runs again after a crash or a lost worker. One that is not, and is not
read-only, is recorded while it runs: if it was interrupted, the task stops and says which step on
which target may have partly run, instead of running it twice. Prefer idempotent steps, such as
`helm upgrade --install` or `terraform apply`.

## Credentials and safety

- **Steps are sandboxes** ([ADR 0008](adr/0008-runner-and-sandbox.md)): no agent login, no SCM token,
  unprivileged. They hold only the environment's `test_secrets`, which the code being deployed can
  read. Scope them to development: a namespace, a dev account or project, never production, and
  never your own administrator credentials.
- **Steps come from the operator's configuration only.** Neither a ticket, nor an agent, nor the
  repository can add a step, a target or a credential. What the deployed code does with the
  credentials is bounded by what they grant, which is why that scope matters.
- `verify` and `evidence` steps must be read-only and destructive steps only release, as the
  configuration is checked when it loads.
- With the [egress proxy](docker-runtime.md#limiting-egress-to-listed-hosts), steps reach only the
  listed hosts, such as the dev cluster's API.

## A staging launcher of your own

Any system with a command-line launcher fits the `command` provider. For one that needs more, an
adapter implements `core.EnvironmentProvider`: `Targets`, `Steps` and `RunStep`. It is held to the
same contract suite as the others; `fake.Environment` is a mocked staging launcher that passes it.
