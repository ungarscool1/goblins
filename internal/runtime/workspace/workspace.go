// Package workspace builds the workloads that manage Git checkouts on volumes. It is
// where the boundary of docs/adr/0004 becomes concrete:
//
//   - A token is only present in Prepare and Publish, and neither ever opens a
//     repository that untrusted code could have touched. Prepare starts from an empty
//     directory. Publish reads a bundle file into a fresh repository, verifies the
//     commit hash, and pushes from there, so hooks and configuration written into the
//     workspace are never honored while a token exists.
//   - Tests run on a separate scratch copy, so repository code cannot alter the
//     workspace that the credential-bearing agent reads and writes next.
//   - Checkouts use core.symlinks=false, so a repository cannot plant a symlink that
//     leads the agent's file tools out of the workspace towards its credentials.
package workspace

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

const (
	// Mount is where the workspace volume appears in a workload.
	Mount = "/workspace"
	// RepoDir is the checkout. It is fixed, because agent sessions are keyed by directory.
	RepoDir = Mount + "/repo"
	// ScratchMount is where a validation copy is prepared.
	ScratchMount = "/scratch"

	resultPrefix = "GOBLINS_RESULT "
)

// Identity attributes commits.
type Identity struct {
	Name    string
	Email   string
	Signoff bool
}

// Result is the machine-readable last line of a workspace operation.
type Result struct {
	Prepared bool   `json:"prepared,omitempty"`
	BaseSHA  string `json:"base_sha,omitempty"`
	HeadSHA  string `json:"head_sha,omitempty"`
	Changed  bool   `json:"changed,omitempty"`
}

// ParseResult reads the result line from a workload's output. It uses the last match,
// because everything before it may be arbitrary tool output.
func ParseResult(stdout string) (Result, error) {
	i := strings.LastIndex(stdout, resultPrefix)
	if i < 0 {
		return Result{}, core.Errorf(core.KindTransient, "workspace.result", "the workspace operation produced no result")
	}
	line := stdout[i+len(resultPrefix):]
	if j := strings.IndexByte(line, '\n'); j >= 0 {
		line = line[:j]
	}
	var r Result
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		return Result{}, core.Errorf(core.KindTransient, "workspace.result", "the workspace operation produced an unreadable result")
	}
	return r, nil
}

var (
	branchRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,200}$`)
	shaRE    = regexp.MustCompile(`^[0-9a-f]{40,64}$`)
)

func validBranch(b string) error {
	if !branchRE.MatchString(b) || strings.Contains(b, "..") || strings.HasSuffix(b, ".lock") {
		return core.Errorf(core.KindInvalid, "workspace", "invalid branch name")
	}
	return nil
}

// BranchName returns the work branch of a task. It is deterministic, so retries
// update the same branch and pull request.
func BranchName(taskID string) string { return "goblins/" + taskID }

const preamble = `set -eu
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0
result() { printf '` + resultPrefix + `%s\n' "$1"; }
`

// The askpass helper hands credentials to Git without placing them in a URL, an
// argument or a file on a volume. /tmp is memory-backed and dies with the workload.
//
//nolint:gosec // a helper script that reads the credential from the environment
const askpass = `tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cat > "$tmp/askpass" <<'EOF_ASKPASS'
#!/bin/sh
case "$1" in
  Username*) printf '%s' "$GIT_USERNAME" ;;
  *) printf '%s' "$GIT_PASSWORD" ;;
esac
EOF_ASKPASS
chmod 700 "$tmp/askpass"
export GIT_ASKPASS="$tmp/askpass"
`

const git = `git -c core.hooksPath=/dev/null -c core.fsmonitor=false -c commit.gpgsign=false -c core.symlinks=false`

func base(name, image, volume string, labels map[string]string, network core.NetworkPolicy, script string, readOnly bool) core.WorkloadSpec {
	return core.WorkloadSpec{
		Name: name, Image: image, Command: []string{"sh", "-c", script}, WorkDir: Mount,
		Mounts: []core.Mount{{Volume: volume, Target: Mount, ReadOnly: readOnly}}, Network: network, Labels: labels,
		Security: core.SecurityProfile{ReadOnlyRoot: true},
		Env:      map[string]string{"HOME": "/tmp"},
		Timeout:  30 * time.Minute,
	}
}

// InspectSpec reports whether a workspace is ready, without any credentials.
func InspectSpec(name, image, volume string, labels map[string]string) core.WorkloadSpec {
	script := preamble + `
if [ -f /workspace/.goblins-base ]; then
  head=$(git -c safe.directory='*' -C /workspace/repo rev-parse HEAD)
  result "{\"prepared\":true,\"base_sha\":\"$(cat /workspace/.goblins-base)\",\"head_sha\":\"$head\"}"
else
  result '{"prepared":false}'
fi
`
	return base(name, image, volume, labels, core.NetworkNone, script, true)
}

// PrepareSpec fetches a pinned commit into an empty workspace. It holds a read token
// and always wipes the directory first, so it never operates on existing content.
func PrepareSpec(name, image, volume string, labels map[string]string, access core.GitAccess, baseBranch, baseSHA, branch string) (core.WorkloadSpec, error) {
	if err := validBranch(baseBranch); err != nil {
		return core.WorkloadSpec{}, err
	}
	if err := validBranch(branch); err != nil {
		return core.WorkloadSpec{}, err
	}
	if !shaRE.MatchString(baseSHA) {
		return core.WorkloadSpec{}, core.Errorf(core.KindInvalid, "workspace.prepare", "invalid base commit")
	}
	script := preamble + askpass + `
cd /workspace
find . -mindepth 1 -maxdepth 1 -exec rm -rf {} +
mkdir repo out
cd repo
git init -q
` + git + ` config core.symlinks false
` + git + ` fetch -q --no-tags "$GOBLINS_REMOTE" "refs/heads/$GOBLINS_BASE_BRANCH"
` + git + ` checkout -q -b "$GOBLINS_BRANCH" "$GOBLINS_BASE_SHA"
printf '%s' "$GOBLINS_BASE_SHA" > /workspace/.goblins-base
result "{\"prepared\":true,\"base_sha\":\"$GOBLINS_BASE_SHA\",\"head_sha\":\"$GOBLINS_BASE_SHA\"}"
`
	s := base(name, image, volume, labels, core.NetworkEgress, script, false)
	s.Env["GOBLINS_REMOTE"] = access.RemoteURL
	s.Env["GOBLINS_BASE_BRANCH"] = baseBranch
	s.Env["GOBLINS_BASE_SHA"] = baseSHA
	s.Env["GOBLINS_BRANCH"] = branch
	s.Env["GIT_USERNAME"] = access.Username
	s.Secrets = map[string]core.Secret{"GIT_PASSWORD": access.Password}
	return s, nil
}

// ReviewTarget is what a reviewer's checkout shows. FetchRef may be empty, and is tried first.
type ReviewTarget struct {
	BaseBranch string
	HeadBranch string
	FetchRef   string
	SHA        string
}

func (t ReviewTarget) valid() error {
	if err := validBranch(t.BaseBranch); err != nil {
		return err
	}
	if err := validBranch(t.HeadBranch); err != nil {
		return err
	}
	if !shaRE.MatchString(t.SHA) {
		return core.Errorf(core.KindInvalid, "workspace.review", "invalid commit")
	}
	if t.FetchRef != "" && (!strings.HasPrefix(t.FetchRef,
		"refs/") || validBranch(t.FetchRef) != nil) {
		return core.Errorf(core.KindInvalid, "workspace.review", "invalid fetch reference")
	}
	return nil
}

// ReviewPrepareSpec fetches a pull request into an empty workspace and checks out one exact
// commit, for a reviewer to read. Like Prepare, it holds a read token, wipes the directory
// first, and executes nothing from the repository.
func ReviewPrepareSpec(name, image, volume string, labels map[string]string, access core.GitAccess,
	t ReviewTarget) (core.WorkloadSpec, error) {
	if err := t.valid(); err != nil {
		return core.WorkloadSpec{}, err
	}
	script := preamble + askpass + `
cd /workspace
find . -mindepth 1 -maxdepth 1 -exec rm -rf {} +
mkdir repo
cd repo
git init -q
` + git + ` config core.symlinks false
` + git + ` fetch -q --no-tags "$GOBLINS_REMOTE" "refs/heads/$GOBLINS_BASE_BRANCH"
base=$(` + git + ` rev-parse FETCH_HEAD)
if [ -z "$GOBLINS_FETCH_REF" ] || ! ` + git + ` fetch -q --no-tags "$GOBLINS_REMOTE" "$GOBLINS_FETCH_REF"; then
  ` + git + ` fetch -q --no-tags "$GOBLINS_REMOTE" "refs/heads/$GOBLINS_BRANCH"
fi
` + git + ` checkout -q --detach "$GOBLINS_SHA"
printf '%s' "$base" > /workspace/.goblins-base
result "{\"prepared\":true,\"base_sha\":\"$base\",\"head_sha\":\"$GOBLINS_SHA\"}"
`
	s := base(name, image, volume, labels, core.NetworkEgress, script, false)
	s.Env["GOBLINS_REMOTE"] = access.RemoteURL
	s.Env["GOBLINS_BASE_BRANCH"] = t.BaseBranch
	s.Env["GOBLINS_BRANCH"] = t.HeadBranch
	s.Env["GOBLINS_FETCH_REF"] = t.FetchRef
	s.Env["GOBLINS_SHA"] = t.SHA
	s.Env["GIT_USERNAME"] = access.Username
	s.Secrets = map[string]core.Secret{"GIT_PASSWORD": access.Password}
	return s, nil
}

// DiffSpec prints what a pull request changes, without credentials or network. External
// diff drivers and text conversions are off, so the repository's attributes run nothing.
func DiffSpec(name, image, volume string, labels map[string]string, sha string,
	maxBytes int) (core.WorkloadSpec, error) {
	if !shaRE.MatchString(sha) {
		return core.WorkloadSpec{}, core.Errorf(core.KindInvalid, "workspace.diff",
			"invalid commit")
	}
	script := preamble + `
cd /workspace/repo
base=$(cat /workspace/.goblins-base)
GIT_OPTIONAL_LOCKS=0 git -c safe.directory='*' diff --no-ext-diff --no-textconv --no-color "$base...$GOBLINS_SHA" | head -c ` + fmt.Sprint(maxBytes) + `
`
	s := base(name, image, volume, labels, core.NetworkNone, script, true)
	s.Env["GOBLINS_SHA"] = sha
	return s, nil
}

// CommitSpec snapshots the agent's changes and writes a bundle of the branch. It has no
// credentials and no network.
func CommitSpec(name, image, volume string, labels map[string]string, id Identity, branch, message string) (core.WorkloadSpec, error) {
	if err := validBranch(branch); err != nil {
		return core.WorkloadSpec{}, err
	}
	signoff := ""
	if id.Signoff {
		signoff = "--signoff"
	}
	script := preamble + `
cd /workspace/repo
base=$(cat /workspace/.goblins-base)
` + git + ` add -A
if ! ` + git + ` diff --cached --quiet; then
  ` + git + ` commit -q --no-verify ` + signoff + ` -m "$GOBLINS_MESSAGE"
fi
head=$(` + git + ` rev-parse HEAD)
changed=true
[ "$head" = "$base" ] && changed=false
mkdir -p /workspace/out
` + git + ` bundle create -q "/workspace/out/$head.tmp" "refs/heads/$GOBLINS_BRANCH" 2>/dev/null || true
if [ -f "/workspace/out/$head.tmp" ]; then mv "/workspace/out/$head.tmp" "/workspace/out/$head.bundle"; fi
result "{\"prepared\":true,\"base_sha\":\"$base\",\"head_sha\":\"$head\",\"changed\":$changed}"
`
	s := base(name, image, volume, labels, core.NetworkNone, script, false)
	s.Env["GOBLINS_MESSAGE"] = message
	s.Env["GOBLINS_BRANCH"] = branch
	s.Env["GIT_AUTHOR_NAME"] = id.Name
	s.Env["GIT_AUTHOR_EMAIL"] = id.Email
	s.Env["GIT_COMMITTER_NAME"] = id.Name
	s.Env["GIT_COMMITTER_EMAIL"] = id.Email
	return s, nil
}

// CopySpec materializes one commit on a scratch volume. Repository code runs there,
// isolated from the workspace; the source is mounted read-only.
func CopySpec(name, image, volume, scratch string, labels map[string]string, sha string) (core.WorkloadSpec, error) {
	if !shaRE.MatchString(sha) {
		return core.WorkloadSpec{}, core.Errorf(core.KindInvalid, "workspace.copy", "invalid commit")
	}
	script := preamble + `
cd /scratch
find . -mindepth 1 -maxdepth 1 -exec rm -rf {} +
git -c safe.directory='*' clone -q --no-hardlinks --no-checkout file:///workspace/repo/.git repo
cd repo
git -c core.hooksPath=/dev/null checkout -q "$GOBLINS_SHA"
result "{\"head_sha\":\"$GOBLINS_SHA\"}"
`
	s := base(name, image, volume, labels, core.NetworkNone, script, true)
	s.WorkDir = ScratchMount
	s.Mounts = append(s.Mounts, core.Mount{Volume: scratch, Target: ScratchMount})
	s.Env["GOBLINS_SHA"] = sha
	return s, nil
}

// PublishSpec pushes a verified commit from a fresh repository. It holds a write token,
// reads nothing from the workspace except the bundle file, and never force-pushes.
func PublishSpec(name, image, volume string, labels map[string]string, access core.GitAccess, baseBranch, branch, sha string) (core.WorkloadSpec, error) {
	if err := validBranch(branch); err != nil {
		return core.WorkloadSpec{}, err
	}
	if !shaRE.MatchString(sha) {
		return core.WorkloadSpec{}, core.Errorf(core.KindInvalid, "workspace.publish", "invalid commit")
	}
	if branch == baseBranch || !strings.HasPrefix(branch, "goblins/") {
		return core.WorkloadSpec{}, core.Errorf(core.KindForbidden, "workspace.publish", "refusing to push to %s: only work branches may be pushed", branch)
	}
	script := preamble + askpass + `
mkdir "$tmp/clean"
cd "$tmp/clean"
git init -q
# The bundle is untrusted data. It is parsed before the token is in scope, with
# object checking on, and its content is identified by the hash recorded earlier.
env -u GIT_PASSWORD -u GIT_USERNAME git -c transfer.fsckObjects=true fetch -q --no-tags "/workspace/out/$GOBLINS_SHA.bundle" "refs/heads/$GOBLINS_BRANCH"
got=$(git rev-parse FETCH_HEAD)
if [ "$got" != "$GOBLINS_SHA" ]; then
  echo "bundle does not contain the expected commit" >&2
  exit 4
fi
git -c core.hooksPath=/dev/null push -q "$GOBLINS_REMOTE" "$GOBLINS_SHA:refs/heads/$GOBLINS_BRANCH"
result "{\"head_sha\":\"$GOBLINS_SHA\"}"
`
	s := base(name, image, volume, labels, core.NetworkEgress, script, true)
	s.Env["GOBLINS_REMOTE"] = access.RemoteURL
	s.Env["GOBLINS_SHA"] = sha
	s.Env["GOBLINS_BRANCH"] = branch
	s.Env["GIT_USERNAME"] = access.Username
	s.Secrets = map[string]core.Secret{"GIT_PASSWORD": access.Password}
	return s, nil
}

// ClassifyPush maps a failed push onto a workflow-relevant error.
func ClassifyPush(stderr string) error {
	switch {
	case strings.Contains(stderr, "does not contain the expected commit"):
		return core.Errorf(core.KindForbidden, "workspace.publish", "the workspace bundle does not match the validated commit")
	case containsAny(stderr, "non-fast-forward", "fetch first", "rejected"):
		if containsAny(stderr, "workflow", "protected branch", "refusing to allow") {
			return core.Errorf(core.KindForbidden, "workspace.publish", "the remote rejected the push; check branch protection and the changed files")
		}
		return core.Errorf(core.KindConflict, "workspace.publish", "the remote branch has diverged, and Goblins does not force-push")
	case containsAny(stderr, "Authentication failed", "could not read Username", "403", "denied", "not found"):
		return core.Errorf(core.KindForbidden, "workspace.publish", "the remote refused the push; check the permissions granted to Goblins")
	}
	return core.Errorf(core.KindTransient, "workspace.publish", "pushing the branch failed")
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// VolumeName derives a volume for a task and purpose.
func VolumeName(installation, taskID, purpose string) string {
	return fmt.Sprintf("goblins-%s-%s-%s", installation, taskID, purpose)
}
