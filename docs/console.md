# Operator console

The console shows every task, its conversation, its agents' transcripts, its design revisions, test
results and history, and lets an operator answer questions, approve a design, pause, resume or
cancel. It has a JSON API with the same content. It is off unless `console.listen` is set, and it
runs on a port of its own. Why it works this way is in
[ADR 0006](adr/0006-operator-console.md).

## Turning it on

```yaml
console:
  listen: 127.0.0.1:8081
  operators:
    - {name: alice, role: operator, token_file: /run/secrets/console-alice}
    - {name: dashboard, role: viewer, token_env: GOBLINS_CONSOLE_VIEWER}
```

| Setting | Default | Meaning |
| --- | --- | --- |
| `console.listen` | off | Address of the console. Must differ from `server.listen` and `server.metrics_listen` |
| `console.operators` | none | Who may log in. At least one when the console is on |
| `operators[].role` | | `operator` can act, `viewer` only reads |
| `operators[].token_env`, `token_file` | | The token, by reference. At least 32 characters |
| `console.read_only` | false | Refuses every action, whatever the role |
| `console.session_ttl` | 12h | How long a browser stays logged in |
| `console.insecure_cookie` | false | Sends the session cookie over plain HTTP. Only for a console reached without TLS on another host than localhost |

Create a token with `openssl rand -base64 32`. `GOBLINS_CONSOLE_LISTEN` overrides `console.listen`.

Keep the console on loopback, or behind a reverse proxy that terminates TLS. Never publish it on
the port that takes webhooks. In mock mode, set `GOBLINS_CONSOLE_LISTEN` and
`GOBLINS_CONSOLE_TOKEN` to try it.

## Pages

- **Tasks** sorts tasks into those that need a person (questions, designs, blocked tasks, pull
  requests awaiting review), those Goblins is working on, and the last 50 finished.
- **A task** shows its state and why, the agents configured for its project, the approved design and
  its pull request, test results, rounds used, the current design with links to earlier revisions,
  the conversation, its jobs and its history.
- **Transcript** shows what agents and checks printed, for the task or one job. It was redacted
  before it was stored.

The conversation is rebuilt from the database: people's comments as Goblins received them, with
the latest edit, answers given in the console, and what Goblins posted. Something Goblins has not
managed to post says so. It survives restarts and replaced containers. Transcripts of finished tasks
are kept for `scheduler.transcript_retention`.

## Actions

| Action | Same as the comment | Notes |
| --- | --- | --- |
| Answer | your text | Goes to the planner. It is not posted on the ticket. It cannot start with `/` |
| Approve revision N | `/approve-design N <hash>` | Names the hash on screen, so a newer revision is refused |
| Pause | `/goblins pause` | Marks the task blocked and stops its job |
| Resume | `/goblins resume` | Runs the interrupted step again, with fresh round budgets |
| Cancel | `/goblins cancel` | |

An action is stored as an operator comment under the operator's name, with maintainer access. The
workflow then applies it, or ignores it, exactly as it would that comment on the ticket. The task's
history and `goblins audit` show who did what. A finished task takes no action.

## API

Send the token as `Authorization: Bearer <token>`.

| Request | Returns |
| --- | --- |
| `GET /api/v1/tasks` | `needs_you`, `working`, `finished` |
| `GET /api/v1/tasks/{id}` | Task, agents, current design, conversation, transitions, jobs, possible actions |
| `GET /api/v1/tasks/{id}/conversation` | The conversation only |
| `GET /api/v1/tasks/{id}/transcript[?job=ID]` | Transcript entries, up to 2000 |
| `GET /api/v1/tasks/{id}/designs/{revision}` | One design revision |
| `POST /api/v1/tasks/{id}/actions` | `202` once stored. Body: `{"action": "answer", "text": "..."}`, `{"action": "approve", "revision": 2, "hash": "..."}`, or `pause`, `resume`, `cancel` |

```bash
curl -s -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8081/api/v1/tasks
```

Refusals say why: `403` for a viewer or a read-only console, `409` for a finished task, `400` for a
malformed action.
