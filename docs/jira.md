# Jira

The `jira` tracker and event source let a work installation take tickets from Jira Cloud or Jira
Data Center. It acts as **one person**, with that person's own access. It never needs a Jira
administrator, a bot account, an app installed by an admin, or a webhook.

Code still lives on an SCM such as GitHub: Jira holds the conversation, the SCM the pull request.

## A work profile

```yaml
installation: {name: work, data_dir: /var/lib/goblins}
trackers:
  jira:
    type: jira
    options:
      url: https://example.atlassian.net
      auth: api_token          # or bearer, or oauth: see below
      email: me@example.com
      token_env: JIRA_TOKEN
      projects: [OPS]          # the only projects Goblins reads
      write: false             # read-only until you say otherwise
      agents: [claude]         # the only agents these tickets may reach
event_sources:
  jira:
    type: jira
    options: {url: https://example.atlassian.net, auth: api_token, email: me@example.com,
              token_env: JIRA_TOKEN, projects: [OPS]}
agents:
  claude: {type: claude-cli, options: {image: goblins-agent-claude:dev, config_volume: work-claude}}
projects:
  - id: ops
    tracker: jira
    tracker_container: OPS     # the Jira project key
    scm: github
    repository: acme/ops-tools
    event_sources: [jira, github]
    poll_interval: 2m
    intake_label: goblins
    stages:
      planner: {agent: claude, runtime: docker}
      implementer: {agent: claude, runtime: docker}
```

Claude is the only agent here: Codex or any other agent is never assumed. `agents` makes that a
rule: Goblins refuses to start if a stage of a project on this tracker names another agent, so a
work ticket cannot reach a personal login by a configuration mistake.

Keep work and personal installations apart, each with its own data directory, logins and
credentials ([ADR 0002](adr/0002-installation-boundaries.md)). Goblins sends ticket text only to
the agents of the installation that read it.

## Credentials

| `auth` | For | Options |
| --- | --- | --- |
| `api_token` | Jira Cloud, if your site's policy allows personal API tokens | `email`, `token_env` |
| `bearer` | A Data Center personal access token, or an OAuth access token you renew yourself | `token_env` |
| `oauth` | Jira Cloud OAuth 2.0 (3LO), with an app you or your admin registered | `client_id`, `client_secret_env`, `refresh_token_file`; `url` is `https://api.atlassian.com/ex/jira/<cloud id>` |

With `oauth`, authorize the app once in a browser and put the refresh token in
`refresh_token_file` (relative paths are in the data directory). Atlassian rotates refresh
tokens: Goblins writes each new one to that file, readable by the service only, before using the
access token that came with it.

Goblins never uses browser cookies or scrapes pages, and never bypasses SSO or MFA. When Jira
refuses the credentials, tasks pause and tell you, and resume once they work again.

`deployment` is `auto` by default, read from `serverInfo`. Set it to `cloud` or `data_center` to
skip that request.

## Reading and writing

By default Goblins only reads. It polls the projects in `projects`: issues updated since the last
poll, their changelog and their comments. The changelog says who changed what, so a label or an
edit counts with the permission of the person who made it, not the reporter's.

With `write: true`, Goblins comments, labels and transitions as the account. Every comment it
writes starts with *Goblins (automated):*, and carries a hidden comment property that makes a
retry find it instead of posting twice. Your own comments, written by hand under the same
account, stay yours: they can approve a design, and Goblins' never can.

Without `write`, nothing is ever sent to Jira except reads. Questions and designs go to the
project's notification provider, or the task blocks and waits for an answer through the
operator inbox:

```bash
goblins comment --config goblins.yaml --task <task-id> --as me --body "/approve-design 1"
```

## Permissions

Approval and input permissions are checked on the issue itself, so issue security levels count:

| Jira permission | Goblins level |
| --- | --- |
| Administer projects | admin |
| Edit issues | write (enough to approve by default) |
| Transition issues | triage |
| Browse projects | read |

## Errors

| You see | Meaning |
| --- | --- |
| `auth_required`: credentials expired or were revoked | Renew the token or authorize the app again |
| `auth_required`: wants a person to log in first (CAPTCHA) | Log in once in a browser |
| `forbidden`: not allowed to do this | The account lacks the Jira permission, such as adding comments |
| `unsupported`: site does not allow this kind of API access | The site disabled API tokens or REST access for this account |
| `unsupported`: `tracker.comment.write` | `write` is off: answer through the operator inbox |
| `rate_limited` | Jira's limit; Goblins waits for the time Jira gave |

## Markup

Comments are converted from Markdown to Jira's wiki markup. Text from tickets, models and test
output is escaped where Jira would act on it: it cannot mention anyone, open a macro, build a
link or table, or embed an image. Designs are shown whole, as Jira has no collapsed section that
every site renders.

## Tests

`internal/providers/jira/fakejira` is an in-memory Jira, in Cloud and Data Center form, with
paging, rate limits, expired credentials and permissions. The tracker contract suite runs against
both, and `test/e2e` takes a Jira ticket to a draft pull request. No test needs a Jira account.
