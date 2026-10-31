# Operator walkthrough

This takes one real issue to a draft pull request. It assumes the installation from
[quickstart.md](quickstart.md) is running. Use a disposable repository: copy `examples/hello`
into a new one, and watch it from `goblins.yaml`, with this check:

```yaml
validations:
  checks:
    type: command
    options:
      image: golang:1.27-alpine
      checks:
        - {name: test, run: "go test ./..."}
```

The offline version of this walkthrough is `test/e2e`, which asserts each step below with a fake
GitHub and a fake CLI.

## The happy path

1. **Open an issue** as someone with write access: *"Add a Greet function that returns a greeting,
   with a test."*
2. **Goblins investigates.** The planner reads the repository, read-only. Nothing is written, and
   no branch exists yet. `goblins status` shows `investigating`.
3. **It may ask questions** in one comment. The task then waits at no cost, for days if needed.
   Reply in a comment. `goblins status` shows `waiting-for-user`.
4. **It proposes a design**, with its scope, architecture, alternatives, constraints, plan, tests,
   security notes and what is out of scope. The comment ends with the exact approval command.
5. **Comment to change it,** which produces a new revision, or **approve it:**

   ```text
   /approve-design 1
   ```

   The command must be alone on the first line of a new comment. Only a person with write access
   can approve, and only the current revision.
6. **The agent implements it** in its own workspace, with no shell. Goblins then commits the result
   and runs your checks on a separate copy, with no credentials and no network. If they fail, the
   output goes back to the agent, for up to three rounds.
7. **A draft pull request appears** on `goblins/<task-id>`, linking the issue and the approved
   design. Review it like any other. **Goblins never merges.**
8. **A status report** on the issue lists the pull request, commit, approver and check results.

## Commands

| Comment           | Effect                                 |
| ----------------- | -------------------------------------- |
| `/goblins pause`  | Stop running work and hold             |
| `/goblins resume` | Continue, granting another set of rounds |
| `/goblins cancel` | Stop and clean up                      |

## When things go wrong

| What happened            | What you see                                                                | What to do |
| ------------------------ | --------------------------------------------------------------------------- | ---------- |
| Login expired            | `quota-paused`, a comment saying a person must log in, and a notification   | Run `docker compose run --rm -it agent-login`. Work resumes on the next timer, or at once with `/goblins resume`. The session continues |
| Usage limit reached      | `quota-paused`, with the reason in `goblins status`                         | Nothing. It retries on its own |
| Tests keep failing       | `blocked`, with "validation still fails" on the issue. Nothing is pushed    | Adjust the design or the tests, then `/goblins resume` |
| Approval missing or wrong | Stays at `design-proposed`; a comment explains a refused approval           | Use the exact command. Nothing runs until then |
| Requirements edited      | The approval is voided, and a new revision is proposed                      | Approve the new revision |
| App lacks a permission   | `blocked`, naming the cause. Work stays on its branch if pushed             | Grant it, then `/goblins resume`. The same pull request is used |
| Process or host restarts | Nothing visible. Interrupted jobs continue from their checkpoint            | Nothing |

`goblins doctor` checks repository access and the agent login at any time, using no model.

## Trying it automatically

`test/smoke/real.sh` runs this scenario against real services. **It spends subscription usage and
writes to GitHub**, so it refuses to run unless you name a disposable repository.

```bash
GOBLINS_SMOKE_REPO=me/disposable test/smoke/real.sh
```
