# Docker runtime

`internal/runtime/docker` implements `ExecutionRuntime` on the Docker Engine HTTP API,
with no CLI in the orchestrator and no assumption that the engine is local.

```yaml
runtimes:
  docker:
    type: docker
    options:
      host: unix:///var/run/docker.sock   # or tcp://socket-proxy:2375; DOCKER_HOST also works
      network: goblins-workers            # bridge for workloads that need egress
      user: "10001:10001"                 # root is refused
      pull_missing: true
      disk_limits: false                  # true only if the storage driver supports size limits
      memory_mb: 4096
      cpus: 2
      pids: 512
```

## What every worker gets

| Setting            | Value                                                        |
| ------------------ | ------------------------------------------------------------ |
| User               | Non-root; root is refused at configuration time              |
| Capabilities       | All dropped, and `no-new-privileges`                         |
| Privileged mode    | Never. A workload asking for it, GPUs or KVM is refused      |
| Mounts             | Named volumes only: a host path, including the Docker socket, cannot be expressed |
| Root filesystem    | Read-only for all Goblins workloads, with a memory-backed `/tmp` |
| Network            | `none`, or a bridge with inter-container traffic disabled    |
| Limits             | Memory (no swap on top), CPU, PIDs; per-container deadline   |

Deadlines are stored as a container label, so they still apply after the orchestrator
restarts. Orphans are found by label and removed by the scheduler.

Disk limits depend on the storage driver (overlay2 on XFS with project quotas, for
example). A workload requesting one is refused unless `disk_limits: true` is set. Named
volumes have no size limit in P0, so a workspace can consume host disk.

## Workspaces

Each task has its own volumes: `plan` for the planner, `impl` for the implementer and
`scratch` for tests. Work state lives on volumes and survives container deletion.

| Step              | Image        | Credentials    | Network | Workspace         |
| ----------------- | ------------ | -------------- | ------- | ----------------- |
| Inspect           | git helper   | None           | None    | Read-only         |
| Prepare           | git helper   | SCM read       | Egress  | Wiped, then writable |
| Agent (planner)   | agent        | Subscription   | Egress  | Read-only         |
| Agent (implementer) | agent      | Subscription   | Egress  | Writable          |
| Commit and bundle | git helper   | None           | None    | Writable          |
| Copy to scratch   | git helper   | None           | None    | Read-only, scratch writable |
| Tests             | project's    | None           | None by default | Scratch only |
| Publish           | git helper   | SCM write      | Egress  | Read-only         |

Checkouts pin the base commit and use a deterministic branch, `goblins/<task-id>`, so
retries update the same branch. See [ADR 0004](adr/0004-agent-credential-boundary.md) for
why the steps are separated.

Pushes are never forced and only target `goblins/*` branches. A diverged branch or a
protected target stops the task rather than overwriting anything.

## Exposing the engine to the orchestrator

Access to the Docker API is root-equivalent on that host. The orchestrator needs it, but
workers never get it. The Compose distribution puts a filtering proxy in between, on an
internal network, which refuses unrelated API areas such as image builds, swarm, secrets and
starting an exec. It filters by path and not by request body, so it reduces what a compromised
orchestrator can do but does not contain it: it could still create a privileged container. For a
stronger boundary, point the runtime at a remote engine dedicated to workers.

## Known limitations

- Workers with egress can reach anything their bridge can route to, including the host
  and its LAN. Per-destination filtering is tracked in #16.
- Workers share the host kernel. For stronger isolation use a dedicated engine host, or a
  runtime such as gVisor configured as the engine default.
- `core.symlinks=false` means repository symlinks appear as small text files in the agent's
  workspace. Test runs happen on a normal clone, where symlinks work.
