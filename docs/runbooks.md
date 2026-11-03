# Runbooks

For the Compose distribution. Run these from `deploy/compose`, with your project name from `.env` in place
of `goblins-personal`. `goblins` below means:

```bash
docker compose exec -T goblins goblins --config /etc/goblins/goblins.yaml
```

Put the subcommand before `--config`, as in `goblins status --config /etc/goblins/goblins.yaml`.

## Find out why a task stopped

1. `goblins status` lists tasks and why each is paused or blocked.
2. `goblins audit --task <id>` gives its transitions and jobs, with errors, and its trace ID.
3. Search the logs for that trace ID: `docker compose logs goblins | grep <trace id>`. Each job attempt
   is a `span` line with its design revision, commit, duration and outcome.

Logs and metrics never hold credentials. Text matching common secret shapes is redacted before it is
logged or stored, which is a safety net that only recognizes those shapes.

## Back up and restore

All state is in the data volume. Stop first for a consistent copy.

```bash
docker compose stop goblins
docker run --rm -v goblins-personal_data:/data:ro -v "$PWD/backups:/out" alpine:3 tar czf /out/goblins.tgz -C /data .
docker compose start goblins
```

To restore, stop Goblins and replace the volume's contents. This discards what is there:

```bash
docker compose stop goblins
docker run --rm -v goblins-personal_data:/data -v "$PWD/backups:/in:ro" alpine:3 sh -c 'rm -rf /data/* /data/.[!.]* ; tar xzf /in/goblins.tgz -C /data'
docker compose start goblins
```

On start, jobs whose lease ran out go back in the queue and workloads no attempt owns are removed.
Anything pushed since the backup stays on the forge, so check open pull requests against `goblins status`.

Logins are in their own volumes (`GOBLINS_AGENT_VOLUME`, `GOBLINS_CODEX_VOLUME`). Leave them out of
backups, and log in again after a restore to a new machine.

## Upgrade

1. Back up, as above. Migrations run on start and only go forward, so going back means restoring.
2. `git pull`, then rebuild what changed:
   `docker compose --profile images build && docker compose build goblins`.
3. `docker compose up -d --force-recreate goblins`.
4. Check `goblins doctor` and `curl -s localhost:8080/readyz`.

Running jobs are requeued when the process stops. Agent CLI versions are pinned in the image files, so
read the release notes before bumping them.

## Recover a broken session

A session that can no longer be resumed starts over from the ticket and approved design on its own, and
the log says "previous agent session is gone". If a task is stuck in another way:

- `/goblins resume` on the ticket retries a blocked or paused task and resets its loop counters.
- `/goblins cancel` ends it and removes its volumes. Anything pushed stays on the forge.

## Rotate GitHub App keys

1. Create a new private key in the App's settings, and keep the old one for now.
2. Replace the file in `secrets/` that `private_key_file` points to.
3. `docker compose up -d --force-recreate goblins`, then run `goblins doctor`.
4. Delete the old key in the App's settings.

To rotate the webhook secret, change it in the App's settings and `GOBLINS_WEBHOOK_SECRET` together. Deliveries
in between are refused and polling picks up what they carried, if `poll_interval` is set.

## An agent login expired

Tasks pause with "agent login is required" and a notification goes out once. Nothing needs cleaning up.

```bash
docker compose run --rm -it agent-login
docker compose --profile codex run --rm -it codex-login
```

Run the one for the agent that needs it. Work retries on its own after the pause, and `/goblins resume`
retries right away.

## Limit usage

- `max_concurrent` on an agent caps how many runs its account has at once.
- `scheduler.max_concurrent_agents` and `max_agents_per_project` cap the rest.
- `timeout` on a stage caps one run, and `policy.max_plan_loops`, `max_implement_loops` and
  `max_review_fix_loops` cap how often a task goes around.
- `/goblins pause` on a ticket stops that task. To stop everything, `docker compose stop goblins`.

## Metrics

Set `server.metrics_listen` to serve Prometheus metrics on `/metrics` at that address, or set
`GOBLINS_METRICS_LISTEN`. Metrics are off by default. They have a port of their own, which must
differ from `server.listen`: that port serves nothing else, and the port that takes webhooks never
serves metrics, so a proxy that lets `/webhooks/` through cannot leak them.

The metrics port has no login and shows project names and counts, so never publish it to the
internet. With Compose, use `metrics_listen: 0.0.0.0:9090` and run Prometheus on the same Docker
network, where it scrapes `goblins:9090` with no port published. To scrape from the host instead,
publish it on loopback only, in a `compose.override.yaml`:

```yaml
services:
  goblins:
    ports: ["127.0.0.1:9090:9090"]
```

| Metric | Meaning |
| --- | --- |
| `goblins_tasks{state}` | Tasks in each state. `quota-paused` shows pauses in force |
| `goblins_jobs{kind,state}` | The queue: pending and running jobs |
| `goblins_tasks_stalled` | Tasks that should be moving and have no job. Means a bug: run an audit on the task the log names |
| `goblins_jobs_total{kind,outcome}` | Finished attempts, where the outcome is `ok` or an error kind |
| `goblins_job_retries_total{kind}` | Attempts after the first |
| `goblins_job_duration_seconds{kind}` | How long attempts take |
| `goblins_task_transitions_total{to}` | State changes, including into `quota-paused` |
| `goblins_events_total{result}` | Events taken in, as `new` or `duplicate` |

Counters start over with each process. Set `scheduler.transcript_retention` to remove transcripts of
finished tasks after that long. By default they stay.
