# Kubernetes

Goblins can run its workloads as Kubernetes pods (`type: kubernetes` runtime), and its control
plane can be installed with the Helm chart in `deploy/helm/goblins`. The two are independent:

| Control plane | Workloads | How |
| --- | --- | --- |
| Helm, in a cluster | Pods in the same cluster | The chart; the runtime uses the pod's service account |
| Docker Compose | Pods in a cluster | `api_url`, a token and `ca_file` for a service account the chart created |
| Helm, in a cluster | A Docker engine | The `docker` runtime with `host: tcp://...`, behind a filtering proxy |

The workflow, the scheduler and the agents see only the runtime contract: nothing in the core
knows which runtime it uses, and both run the same contract suite.

## The runtime

```yaml
runtimes:
  k8s:
    type: kubernetes
    options:
      namespace: goblins-workloads     # required outside a cluster
      service_account: goblins-workload
      network_policies: true           # the cluster enforces NetworkPolicy
      storage_class: ""                # for workspace and login claims
      volume_size: 10Gi
      memory_mb: 4096
      cpus: 2
      # Outside the cluster only:
      # api_url: https://cluster.example:6443
      # token_env: GOBLINS_K8S_TOKEN    # or token_file, read again for every request
      # ca_file: /run/secrets/k8s-ca.crt
```

| Option | Meaning |
| --- | --- |
| `network_policies` | Must be true for workloads that need no network, such as checks. Without it they are refused, since isolation could not be promised |
| `egress_except` | CIDRs agents may not reach. By default private ranges, carrier-grade NAT and the metadata address |
| `disk_limits` | Sets `ephemeral-storage` limits. On by default |
| `gpu_resource`, `kvm_resource` | Extended resource names offered by device plugins, such as `devices.kubevirt.io/kvm`. Without them, GPU and KVM workloads are refused |
| `split_streams` | Asks for stdout and stderr apart, which needs the `PodLogsQuerySplitStreams` feature. Without it, both get the merged log |
| `pull_policy`, `tmp_size`, `uid` | Image pull policy, size of `/tmp`, user ID (never 0) |

### What every pod gets

- One container, `restartPolicy: Never`, and `activeDeadlineSeconds` from the job's timeout.
- No service account token (`automountServiceAccountToken: false`), no service links, and the
  workload service account, which has nothing bound to it.
- Non-root, user 10001, no privilege escalation, every capability dropped, the runtime's
  seccomp profile, a read-only root filesystem when asked, and `/tmp` as a sized `emptyDir`.
  This passes the `restricted` Pod Security Standard, which the chart enforces on the namespace.
- CPU and memory limits, and an ephemeral storage limit when the workload has one.
- A label for its network: `none` pods may reach nothing, `egress` pods only DNS and addresses
  outside `egress_except`.
- Secrets in a Secret object of its own, referenced by key, owned by the pod and deleted with it.
  Their values are never in the pod, where anyone allowed to read pods would see them.

Privileged pods are never offered. Pod IDs per pod cannot be limited through the API: set
`podPidsLimit` on the kubelet.

### Workspaces and restarts

Durable volumes are PersistentVolumeClaims, created when first used, so a workspace survives a pod
being replaced, a node draining and a control plane restart. Nothing assumes a directory on a node.
Workloads are found again by label after a restart: those no live job owns are removed, and the
claims of finished tasks are reclaimed. A `helm upgrade` or `kubectl rollout restart` loses nothing,
as all state is in the database.

### Logs

The API server gives one log for both streams, so without `split_streams` the merged log goes to
both readers. Agents' parsers skip lines that are not theirs, and error checks see everything.

## The chart

```bash
helm install goblins deploy/helm/goblins --namespace goblins --create-namespace \
  --set existingSecret=goblins-env --set-file config=goblins.yaml
```

It creates:

- the control plane, as a Deployment with one replica on SQLite (a claim kept on uninstall), or
  several on PostgreSQL, external (`storage.postgres.dsnSecret`) or inside the chart
  (`postgresql.enabled`). It is non-root with a read-only root filesystem.
- a **workloads namespace** labeled `pod-security.kubernetes.io/enforce: restricted`;
- a **Role in that namespace only**, bound to the control plane: pods (create, get, list,
  watch, patch, delete), their logs, Secrets (create, patch, delete), claims (create, get, list,
  delete) and reading network policies. No cluster-wide permission, no `exec`, no `attach`, nothing
  in the control plane's own namespace;
- a **default-deny** network policy for the workloads namespace, and the two policies pods select;
- a Service, an optional Ingress exposing `/webhooks/` only, and an optional metrics Service.

`config` is your `goblins.yaml`, rendered with `tpl`. Secrets go in `existingSecret`, as
environment variables, or `secretFiles`, mounted under `/run/secrets/<name>`, and the
configuration refers to them by `*_env` or `*_file`.

## Subscription logins in a cluster

An agent's subscription login is the most valuable thing a workload holds. On Kubernetes:

| Concern | How it is handled |
| --- | --- |
| Where it is stored | A claim of its own, the agent's `config_volume`, in the workloads namespace |
| Who mounts it | Agent pods only. Checks, which run a change's code, never mount it and have no network |
| What can read it in an agent pod | The CLI, in restricted mode with no command-running tool, its file tools confined to the workspace ([ADR 0004](adr/0004-agent-credential-boundary.md)) |
| Who else can read it | Anyone allowed to create pods mounting claims in the workloads namespace, and node administrators. Keep the namespace to Goblins |
| Logging in | `agentLogin.enabled=true` creates a login pod: `kubectl -n <ns> attach -it goblins-agent-login`, then disable it again |
| Refreshing | The CLI refreshes its own login inside agent pods, in the same claim |
| Expiry | A task pauses and an operator is told, as on Docker; nothing logs in unattended |
| A token instead | `oauth_token_env` on the agent, from `existingSecret`. It reaches agent pods as a Secret reference, never in the pod spec |
| Separate installations | One namespace, claim and release per installation, as with Compose projects |

A `ReadWriteOnce` claim is attached to one node at a time, so two agent runs sharing a login are
serialized by the scheduler's per-agent limit (`max_concurrent: 1`). Raise it only with a storage
class offering `ReadWriteMany`.

## Tests

`internal/runtime/kubernetes/fakek8s` is an API server for one namespace, enforcing the `restricted`
standard and simulating the pod lifecycle. The runtime contract suite runs against it, as it does
against Docker. `test/helm` renders the chart and checks it asks for no cluster-wide access and
stays restricted; it needs the `helm` binary. No test needs a cluster.
