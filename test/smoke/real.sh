#!/bin/sh
# Opt-in end-to-end smoke test against REAL services, on a DISPOSABLE repository.
#
# It SPENDS SUBSCRIPTION USAGE and writes to GitHub: it opens an issue, answers the planner,
# approves the design and waits for a draft pull request, which it leaves for you to inspect.
# Prerequisites: a running installation following docs/quickstart.md, watching that repository,
# and `gh` logged in as someone with write access to it.
#
#   GOBLINS_SMOKE_REPO=me/disposable test/smoke/real.sh
#
# It refuses to run without that variable, and has never been run in CI.
set -eu
: "${GOBLINS_SMOKE_REPO:?set GOBLINS_SMOKE_REPO to a disposable repository}"
repo=$GOBLINS_SMOKE_REPO
timeout=${GOBLINS_SMOKE_TIMEOUT:-1800}

last_bot_comment() {
  gh issue view "$issue" --repo "$repo" --json comments \
    --jq '[.comments[] | select(.author.login | test("\\[bot\\]$|^app/|goblins"))] | last | .body // ""'
}

title="Goblins smoke test $(date +%s)"
url=$(gh issue create --repo "$repo" --title "$title" --body "Add a function Greet(name) that returns \"Hello, name!\", with a test. Keep it minimal. There are no other requirements, so please do not ask questions.")
issue=${url##*/}
echo "opened $url"

start=$(date +%s)
answered=0
approved=0
while :; do
  now=$(date +%s)
  if [ $((now - start)) -gt "$timeout" ]; then
    echo "timed out; see $url" >&2
    exit 1
  fi
  body=$(last_bot_comment)
  case "$body" in
    *"Goblins is blocked"*|*"stopped working"*)
      echo "the task stopped: $body" >&2
      exit 1 ;;
    *"Status report"*)
      break ;;
    *"Proposed design, revision "*)
      if [ "$approved" = 0 ]; then
        rev=$(printf '%s' "$body" | sed -n 's/.*Proposed design, revision \([0-9]*\).*/\1/p' | head -1)
        gh issue comment "$issue" --repo "$repo" --body "/approve-design $rev" >/dev/null
        approved=1
        echo "approved revision $rev"
      fi ;;
    *"I have some questions"*)
      if [ "$answered" -lt 3 ]; then
        gh issue comment "$issue" --repo "$repo" --body "Keep it as simple as possible; pick sensible defaults." >/dev/null
        answered=$((answered + 1))
        echo "answered a question"
      fi ;;
  esac
  sleep 30
done

pr=$(gh pr list --repo "$repo" --state open --search "$title in:title" --json number,isDraft,headRefName --jq '.[0]')
echo "pull request: $pr"
case "$pr" in
  *'"isDraft":true'*) ;;
  *) echo "expected one draft pull request" >&2; exit 1 ;;
esac
case "$pr" in
  *'"headRefName":"goblins/'*) ;;
  *) echo "unexpected branch" >&2; exit 1 ;;
esac
echo "ok: inspect $url and the pull request, then close both"
