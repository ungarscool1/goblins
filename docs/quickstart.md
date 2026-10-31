# Quick start

## Mock mode

Try the whole workflow with fake providers. This needs Docker Compose and nothing else: no
credentials, no agent login and no access to the Docker socket.

```bash
cd deploy/compose && docker compose up -d --build
```

```bash
curl -s localhost:8080/mock/demo/issues -d '{"title":"Add a thing","body":"Please"}'
```

```bash
docker compose exec goblins goblins status --mock
```

```bash
curl -s localhost:8080/mock/demo/issues/1/comments -d '{"body":"/approve-design 1"}'
```

Tasks survive `docker compose restart`. The fake tracker's contents are kept in memory, so a
task that needs them again after a restart becomes `blocked`.

## A real installation

You need a dedicated machine or VM running Docker with Docker Compose (see "Where to run it" under
[Operations](#operations)), a repository you control, and a paid
Claude subscription. **No model API key is used anywhere.** Kubernetes is not required.

### 1. Copy the configuration

```bash
cd deploy/compose && cp .env.example .env && cp goblins.example.yaml goblins.yaml
```

Edit `goblins.yaml` wherever it says `CHANGE ME`, and `.env`.

### 2. Create the GitHub App

Follow [github.md](github.md). Store the key where the non-root orchestrator can read it:

```bash
mkdir -p secrets && cp ~/Downloads/your-app.private-key.pem secrets/github-app.pem
```

```bash
sudo chown 65532 secrets/github-app.pem && sudo chmod 400 secrets/github-app.pem
```

### 3. Build the images

```bash
docker compose --profile images build
```

```bash
docker compose build
```

### 4. Log in to Claude

This runs in a container, and the login is stored only in this installation's volume:

```bash
docker compose run --rm -it agent-login
```

### 5. Start and check

```bash
docker compose up -d
```

```bash
docker compose exec goblins goblins doctor --config /etc/goblins/goblins.yaml
```

`doctor` checks repository access and the agent login. It uses no model.

### 6. Give it work

Open an issue on the repository as someone with write access. Goblins investigates, asks
questions in the issue, and proposes a design. Comment `/approve-design <revision>` to
start implementation. A draft pull request and a status report follow. **Nothing merges
automatically.**

```bash
docker compose exec goblins goblins status --config /etc/goblins/goblins.yaml
```

### What can go wrong

| Symptom                               | Meaning and fix                                       |
| ------------------------------------- | ----------------------------------------------------- |
| Task is `quota-paused`, login message | Run `agent-login` again; the task resumes on its own  |
| Task is `quota-paused`, usage limit   | Nothing to do; it retries after the pause             |
| Task is `blocked`                     | The reason is on the ticket and in `goblins status`. Fix it, then comment `/goblins resume` |
| Tests fail repeatedly                 | `blocked` after the configured rounds, with the output on the ticket. Nothing is published |
| Approval refused                      | The comment explains why: wrong revision, or not enough access |

## Separate home and work installations

An installation shares nothing with another: its own Compose project, database, agent login and
configuration. Clone the repository twice, or use two directories, and give each its own `.env`:

```bash
GOBLINS_PROJECT=goblins-work
GOBLINS_INSTALLATION=work
GOBLINS_AGENT_VOLUME=goblins-work-claude-config
GOBLINS_PORT=8081
```

Log in with the work account inside the work directory. Keep `config_volume`,
`installation.name` and the workers' `network` distinct in its `goblins.yaml`. A separate worker
network and installation label keep each instance from touching the other's workloads, even on
the same engine. A Jira tracker for work is planned in #15; until then, a tracker with no adapter
cannot be configured.

## Operations

- **Exposure.** The port is published on loopback only. Only `POST /webhooks/*` should reach
  the internet, through a reverse proxy or a tunnel. See [github.md](github.md).
- **Docker access.** The orchestrator reaches the engine through a filtering proxy on an internal
  network. It can still create containers, including privileged ones, which is root-equivalent on
  that host, so treat the orchestrator accordingly. Workers never get access. See
  [docker-runtime.md](docker-runtime.md).
- **Where to run it.** Use a dedicated, disposable host or VM, with nothing else on its Docker
  engine. The proxy filters API paths, not what `/containers/create` asks for, so it does not
  protect other services on the engine from a compromised orchestrator. Nothing enforces this
  for you yet; a policy-enforcing boundary is tracked in #16.
- **Backups.** Everything durable is in the `data` volume and the agent config volume.

  ```bash
  docker compose stop goblins
  ```

  ```bash
  docker run --rm -v goblins-personal_data:/data:ro -v "$PWD":/backup alpine tar czf /backup/goblins-data.tgz -C /data .
  ```

  ```bash
  docker compose start goblins
  ```

  The agent config volume holds the subscription login. Protect it, or skip it and log in again.
- **Upgrades.** Pull the new version and rebuild. Migrations run at startup and only go forward;
  an older binary refuses a newer database, so back up first. Running jobs are requeued on
  shutdown and continue from their checkpoint.
- **Agent image.** It pins a CLI version. Rebuild deliberately: the adapter checks at runtime that
  the CLI still has the options the security model relies on, and blocks tasks if not.
- **Postgres.** Set `storage.driver: postgres` and `storage.dsn_env`, and run your own server.
  It is only useful for several replicas, which Compose does not need.
