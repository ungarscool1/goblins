#!/bin/sh
# Starts the mock distribution with Docker Compose and walks through the acceptance
# criteria: it starts, it reports healthy, a task progresses, and restarting keeps state.
# Uses no credentials and no tokens.
set -eu
cd "$(dirname "$0")/../../deploy/compose"
export GOBLINS_PROJECT="goblins-smoke-$$" GOBLINS_PORT="${GOBLINS_PORT:-18080}"
url="http://127.0.0.1:$GOBLINS_PORT"
cleanup() { docker compose down -v --remove-orphans >/dev/null 2>&1 || true; }
trap cleanup EXIT

wait_for() {
  i=0
  until eval "$1"; do
    i=$((i + 1))
    if [ "$i" -gt 60 ]; then
      echo "timed out: $1" >&2
      docker compose logs >&2
      exit 1
    fi
    sleep 1
  done
}
status() { docker compose exec -T goblins goblins status --mock; }

docker compose up -d --build --quiet-pull
wait_for '[ "$(docker inspect -f "{{.State.Health.Status}}" "$(docker compose ps -q goblins)")" = healthy ]'
echo "healthy"

curl -fsS "$url/mock/demo/issues" -d '{"title":"Smoke","body":"Test"}' >/dev/null
wait_for 'status | grep -q design-proposed'
echo "design proposed"

docker compose restart goblins >/dev/null
wait_for 'curl -fsS "$url/readyz" >/dev/null 2>&1'
status | grep -q design-proposed
echo "state survived a restart"

if docker inspect -f '{{range .Mounts}}{{.Source}} {{end}}' "$(docker compose ps -q goblins)" | grep -q docker.sock; then
  echo "mock mode must not mount the Docker socket" >&2
  exit 1
fi
echo "ok"
