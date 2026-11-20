# Blueprint, approval profiles and tools

## The blueprint

Every task runs one blueprint, `issue-to-draft@1`. Its stages are typed, and deterministic code
owns every transition between them: model output is data, never workflow control.

| Stage | Type | Driven by | Bounded by |
| --- | --- | --- | --- |
| Plan | agent, read-only | `stages.planner` | `max_plan_loops` |
| Approve | approval | the project's approval profile, below | a person, or the profile's scope |
| Implement | agent, file tools only | `stages.implementer` | `max_implement_loops`, `max_tool_calls`, `timeout` |
| Check | command | the validation provider or the repository's manifest ([configuration.md](configuration.md#validation)) | per-check `timeout` |
| Deploy and verify | environment action | the project's environment ([environments.md](environments.md)) | its steps' kinds and the lease |
| Review | review | `review`, `stages.reviewers` | `max_review_fix_loops`, `max_triage_rounds` |
| Publish | publish | the SCM, as a draft | only the commit that passed every stage before |

A failed check, deployment or review goes back to Implement with its evidence, within the bounds,
and then blocks for a person. Mandatory stages always run against the exact commit being
published. Projects choose what runs inside each stage, never the order, or whether a gate
applies: those belong to the blueprint, which a ticket, an agent or the repository cannot change
([ADR 0009](adr/0009-blueprint-and-approval-profiles.md)). Templates and task graphs across
projects are #25.

The transcript records, for each agent run, the blueprint, the agent and its type, model, effort,
permissions, allow-listed tools and runtime.

## Approval profiles

```yaml
policy:
  intake_label: goblins
  approval:
    profile: unattended            # strict, the default, waits for a person on every design
    label: "goblins:unattended"    # a person with approval permission applies it, per item
    allowed_paths: ["docs/**", "src/content/**"]
    denied_paths: ["src/content/legal/**"]
    max_changed_files: 10
```

**Strict** is the default: every design waits for `/approve-design`. **Unattended** is an
operator's explicit choice for a constrained scope:

- A person with `min_approve_permission` applies the label to an item; it also starts work. A
  label by anyone else, or by automation, does nothing, and the intake label never counts.
- The design is still posted. Once it is on the ticket, it is approved by the profile, recorded
  with the profile's hash and the person who applied the label.
- After each implementation round, before any check, Goblins compares the paths the commit
  changed with the scope: every path must match `allowed_paths`, none may match `denied_paths`,
  the [built-in sensitive paths](#built-in-sensitive-paths), `trusted_paths` or the validation
  manifest, and the count must stay within `max_changed_files`. This is decided from the commit,
  never from what a ticket or a model says about it.
- A change outside the scope stops: the approval is withdrawn, the comment names the paths,
  nothing is published, and the design waits for `/approve-design`. A person's approval then
  checks that same commit. From then on, the task needs a person like any other.
- Checks, environment verification, review and the draft pull request are unchanged: an
  unattended task cannot skip a gate, and nothing merges.

### Built-in sensitive paths

Automation (`.github/**`, `.gitlab-ci.yml`), Goblins' files (`.goblins/**`), containers and
infrastructure (`**/Dockerfile`, `**/*.tf`, `**/*.tfvars`, `deploy/**`, `infra/**`, `terraform/**`,
`charts/**`, `helm/**`), authentication and credentials (`**/auth/**`, `**/security/**`,
`**/*secret*`, `**/*credential*`, `**/.env*`, `**/*.pem`, `**/*.key`) and dependencies (`go.mod`,
`go.sum`, `package.json`, lock files). Matching ignores case.

## Allow-listed tools

Agents have file tools only, by role ([ADR 0004](adr/0004-agent-credential-boundary.md)). An
operator can add MCP servers that are already installed in the agent image, and give them to
stages by name:

```yaml
agents:
  claude:
    type: claude-cli
    options:
      image: my-agent-with-mcp:1.4.2
      mcp_servers:
        docs: {command: /opt/mcp/docs-search, args: [--read-only]}
projects:
  - stages:
      planner: {agent: claude, runtime: docker, tools: [docs]}
      implementer: {agent: claude, runtime: docker}
```

- A stage gets only the servers it names, in a configuration Goblins writes; the repository's own
  MCP configuration, hooks and skills are ignored either way (`--strict-mcp-config`,
  `--restricted`, `--disable-slash-commands`).
- A stage naming a tool its agent does not offer, or an agent that offers none, is refused at
  startup. An agent that cannot run tools at all (Codex today) reports `agent.tools` as
  unsupported.
- **An MCP server runs inside the agent's workload, next to its login.** Only add servers you
  would trust with it: read-only ones that cannot run commands or reach arbitrary hosts. Values in
  `env` are not secrets: the configuration refuses inline secrets.
- Nothing is installed at run time: what is offered is what the image contains, at the version
  the image pins.
