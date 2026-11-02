# GitHub setup

Goblins acts on GitHub as a **GitHub App**. The App's private key and webhook secret
are GitHub credentials; they have nothing to do with model access, and GitHub Actions
is not involved in invoking any model.

## Register the App

Create it under your account or organization: **Settings → Developer settings →
GitHub Apps → New GitHub App**.

- **Webhook URL:** `https://<your-host>/webhooks/<event-source-name>`, or disable
  webhooks and rely on polling (see below).
- **Webhook secret:** a long random value, passed to Goblins via `webhook_secret_env`.
- **Where can it be installed:** only on this account.

### Repository permissions

These are the minimum Goblins uses:

| Permission      | Access       | Used for                                   |
| --------------- | ------------ | ------------------------------------------ |
| Metadata        | Read         | Required by GitHub; also collaborator permission for approvals |
| Issues          | Read & write | Reading issues, posting questions, designs |
| Pull requests   | Read & write | Draft pull requests, comments, reviews     |
| Contents        | Read & write | Cloning, pushing work branches             |
| Checks          | Read         | Optional: reading CI results               |

Whether a review thread is resolved comes from the GraphQL API, which the Pull requests
permission also covers.

Do not grant **Workflows**. Without it GitHub refuses a push that changes
`.github/workflows`, which is a useful limit on what an agent's work can do.

If reading collaborator permissions fails, approvals fail closed: nobody can approve.

### Events to subscribe to

Issues, Issue comment, Pull request, Pull request review, Check run.

### Install it

Generate a private key, install the App on the repositories you want, and keep the key
outside the repository, for instance at a path mounted as a Docker secret.

## Configure Goblins

```yaml
trackers:
  forge:
    type: github
    options: {app_id: 123456, private_key_file: /run/secrets/github-app.pem}
scms:
  forge:
    type: github
    options: {app_id: 123456, private_key_file: /run/secrets/github-app.pem}
event_sources:
  forge:
    type: github
    options:
      webhook_secret_env: GOBLINS_GITHUB_WEBHOOK_SECRET
      app_id: 123456
      private_key_file: /run/secrets/github-app.pem
      repositories: [me/app]
      installation_ids: ["7654321"]   # optional second allow-list
```

## Reaching Goblins from GitHub

Only `/webhooks/*` needs to be reachable. Publish it through a reverse proxy, or a
tunnel such as Cloudflare Tunnel or Tailscale Funnel. **Do not expose the rest of the
HTTP port.**

With no inbound path at all, leave `webhook_secret_env` out. The source then advertises
polling only, and the scheduler reads changes on an interval. In this mode a comment
edited before it is read counts as an edit, so it can give input but cannot approve or
run a command. Polling catches up gradually after a long gap, but stops with a logged
error if more than 2000 issue events or open pull requests changed since its last read,
because those listings cannot resume. Polling is read-only and
has two limits: an edit is attributed to the issue's author, because the API does not
say who edited it; and comment edits only surface as new timestamps. Webhooks and
polling can run together, since one fact has one identity and is processed once.

## Security properties

- Webhooks are authenticated with `X-Hub-Signature-256` using a constant-time compare,
  before anything in the payload is trusted, and bodies are bounded.
- Deliveries for repositories or installations outside the allow-lists are refused.
- Installation tokens are requested per repository and rotated before they expire.
- Redeliveries and replays of a signed payload collapse on the event's identity.
- Comments by Bot accounts never trigger work, which is what prevents loops.
- Comment idempotency markers only count on comments by Bot accounts, so a person
  cannot suppress a comment by pasting a marker.

## Pull requests

- Work is pushed to `goblins/<task-id>`, never to a default or protected branch and never
  with force. A diverged branch blocks the task rather than overwriting anything.
- One head has one open pull request. Later iterations update it.
- Pull requests are opened as drafts. On a private repository whose plan lacks draft pull
  requests, GitHub refuses them, and the task becomes `blocked` with that reason.
- A missing permission blocks the task and names the cause. Work stays on its branch.
- Goblins never merges in P0. It also never submits an approving review, which could
  otherwise satisfy branch protection by itself: an approval verdict is posted as a comment.
- Asking another service's bot for a review by comment, such as `@codex review`, is not
  implemented. It has not been verified that a bot comment triggers one, so review goes
  through the provider-independent review stage (#13, #19).

## Following pull requests

To follow what happens after the report, the App also subscribes to `Pull request`, `Pull request
review`, `Pull request review comment` and `Check run` events; polling observes the same facts. It
reads reviews, review comments and check runs, which `Pull requests: read` and `Checks: read` allow.

Reading branch protection needs `Administration: read`. Without it the protection is reported as
unknown, which `require_branch_protection` treats as not protected, and required checks from
protection cannot be added to the gate. Configure `required_checks` in that case.

Merging needs `Contents: write` and is used only by an `auto_merge` rule. Goblins publishes reviews as
comments, so it can neither satisfy nor block branch protection with its own reviews.
