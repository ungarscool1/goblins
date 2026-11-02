# Providers and extension points

Everything that talks to the outside world is an adapter behind a contract in
`internal/core/contracts.go`. Core packages know contracts only; `internal/archtest`
fails the build if a core package names a product or if an adapter imports the
workflow or storage packages.

| Kind           | Contract               | Responsibility                                           |
| -------------- | ---------------------- | -------------------------------------------------------- |
| `tracker`      | `IssueTracker`         | Issues, comments, labels, transitions, actor permissions |
| `scm`          | `SourceControl`        | Git access, branches, commits, PR/MR, reviews, checks    |
| `event_source` | `EventSource`          | Normalizes webhooks and polling into canonical events    |
| `agent`        | `AgentProvider`        | Launch and resume conversations, auth state, models      |
| `runtime`      | `ExecutionRuntime`     | Isolated workloads and volumes                           |
| `review`       | `ReviewProvider`       | Independent review of one exact commit of a pull request |
| `validation`   | `ValidationProvider`   | Deterministic checks gating the workflow                 |
| `notification` | `NotificationProvider` | Operator messages outside the ticket                     |

Persistence has its own contract, `StateStore`, which adapters never receive.

## Rules for adapters

- **Report facts, do not decide.** Adapter factories receive `providers.Deps`,
  which has no store and no engine. Only the workflow engine moves tasks.
- **Capabilities are explicit.** `Info().Capabilities` lists optional features.
  An operation whose capability is not advertised returns `KindUnsupported`, never
  a silent success. Orchestration calls `core.Negotiate` and turns a refusal into
  an explicit `blocked` task state whose reason is shown to the user.
- **Effects are idempotent.** Comments, pull requests, reviews and notifications
  carry idempotency keys, because jobs are retried after crashes.
- **Event identity is the fact, not the delivery.** `DedupKey` is the same for a
  webhook, its redelivery and a later poll reporting the same fact.
- **Errors are classified.** Use `core.Error` kinds. `KindQuotaExhausted` and
  `KindAuthRequired` pause tasks rather than failing them.
- **Secrets are wrapped.** `core.Secret` cannot be printed or serialized.

## Adding an adapter

1. Implement the contract in `internal/providers/<name>`.
2. Register its factory in `internal/providers/builtin`.
3. Call the suite from `internal/providers/contract` in your test package.

No workflow code changes. `contract/fake_test.go` shows a small independent
tracker passing the suite with read-only capabilities.

## Review conversations

`SourceControl` carries what review needs, gated where SCMs differ: `ReviewThreads`, `ListReviews`,
`PullRequestComments`, `ChangedFiles`, plus `ReplyToThread` (`scm.review_threads.reply`),
`BranchProtection` (`scm.branch_protection`) and `MergePullRequest` (`scm.merge`). A provider without a
capability gets a visible blocked state, not a guess. Threads are identified by their first remark.

## Versioning

`core.ContractVersion` is bumped on breaking changes to the contracts. Adapters
report the version they target and the registry refuses a mismatch at startup.
Adding a capability constant is not a breaking change.

## Future: out-of-process adapters

Adapters are compiled in for now. Arbitrary executable plugins are deliberately
not supported: a plugin would run with control-plane privileges and credentials.
When demand exists (#24), the plan is a versioned gRPC protocol mirroring these
contracts one-to-one, with a Go shim that registers a remote adapter as a normal
factory, so that `Info()` negotiation and the contract suites apply unchanged.

## Example installations

The same project shape works for any combination, because only `type` differs:

```yaml
# Home: tracker and SCM are both the same forge, split into two instances.
trackers:      {forge: {type: github, options: {app_id: 123, private_key_file: /run/secrets/app.pem}}}
scms:          {forge: {type: github, options: {app_id: 123, private_key_file: /run/secrets/app.pem}}}
event_sources: {forge: {type: github, options: {webhook_secret_env: GH_WEBHOOK_SECRET}}}
agents:        {claude: {type: claude-cli}}
runtimes:      {docker: {type: docker}}
projects:
  - id: app
    tracker: forge
    scm: forge
    event_sources: [forge]
    repository: me/app
    stages:
      planner:     {agent: claude, runtime: docker}
      implementer: {agent: claude, runtime: docker, effort: high}
```

```yaml
# Work: another installation with its own login, tracker and data. Only the
# tracker differs; the Jira adapter is planned in #15 and not available yet.
trackers:      {jira: {type: jira, options: {site: example.atlassian.net}}}
scms:          {forge: {type: github, options: {app_id: 456, private_key_file: /run/secrets/app.pem}}}
agents:        {claude: {type: claude-cli}}
runtimes:      {docker: {type: docker}}
projects:
  - id: service
    tracker: jira
    scm: forge
    repository: corp/service
    stages:
      planner:     {agent: claude, runtime: docker}
      implementer: {agent: claude, runtime: docker}
```
