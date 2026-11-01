package workspace_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers/fake"
	"github.com/ungarscool1/goblins/internal/runtime"
	"github.com/ungarscool1/goblins/internal/runtime/workspace"
)

var ctx = context.Background()

var id = workspace.Identity{Name: "Goblins", Email: "goblins@example.invalid", Signoff: true}

type world struct {
	t      *testing.T
	rt     *fake.ProcessRuntime
	remote string
	base   string
	access core.GitAccess
	n      int
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@e", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func setup(t *testing.T) *world {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	gitOut(t, root, "init", "-q", "--bare", "-b", "main", remote)
	gitOut(t, root, "init", "-q", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(seed, "evil-link")); err != nil {
		t.Fatal(err)
	}
	gitOut(t, seed, "add", "-A")
	gitOut(t, seed, "commit", "-qm", "init")
	gitOut(t, seed, "push", "-q", remote, "main")
	return &world{t: t, rt: fake.NewProcessRuntime("p", t.TempDir(), ""), remote: remote, base: gitOut(t, seed, "rev-parse", "HEAD"),
		access: core.GitAccess{RemoteURL: "file://" + remote, Username: "x", Password: core.NewSecret("s3cret-token")}}
}

func (w *world) run(spec core.WorkloadSpec, err error) workspace.Result {
	w.t.Helper()
	if err != nil {
		w.t.Fatal(err)
	}
	w.n++
	spec.Name = spec.Name + "-" + string(rune('a'+w.n))
	res, err := runtime.RunToCompletion(ctx, w.rt, spec)
	if err != nil {
		w.t.Fatal(err)
	}
	if res.Status.Phase != core.WorkloadSucceeded {
		w.t.Fatalf("%s failed: %+v\nstdout: %s\nstderr: %s", spec.Name, res.Status, res.Stdout, res.Stderr)
	}
	r, err := workspace.ParseResult(res.Stdout)
	if err != nil {
		w.t.Fatalf("%v\n%s", err, res.Stdout)
	}
	return r
}

func (w *world) prepare(vol, branch string) {
	w.t.Helper()
	got := w.run(workspace.PrepareSpec("prep", "img", vol, nil, w.access, "main", w.base, branch))
	if !got.Prepared || got.BaseSHA != w.base {
		w.t.Fatalf("prepare = %+v", got)
	}
}

func TestFullCycleRealGit(t *testing.T) {
	w := setup(t)
	if got := w.run(workspace.InspectSpec("inspect", "img", "ws", nil), nil); got.Prepared {
		t.Fatal("empty workspace reported as prepared")
	}
	w.prepare("ws", "goblins/t1")
	if got := w.run(workspace.InspectSpec("inspect", "img", "ws", nil), nil); !got.Prepared || got.BaseSHA != w.base {
		t.Fatalf("inspect = %+v", got)
	}
	repo := filepath.Join(w.rt.VolumePath("ws"), "repo")
	if gitOut(t, repo, "rev-parse", "HEAD") != w.base {
		t.Fatal("not pinned to the base commit")
	}

	// No changes yet: not changed.
	got := w.run(workspace.CommitSpec("commit", "img", "ws", nil, id, "goblins/t1", "msg"))
	if got.Changed || got.HeadSHA != w.base {
		t.Fatalf("empty commit = %+v", got)
	}

	// The agent edits a file.
	if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("feature\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got = w.run(workspace.CommitSpec("commit", "img", "ws", nil, id, "goblins/t1", "Implement feature"))
	if !got.Changed || got.HeadSHA == w.base {
		t.Fatalf("commit = %+v", got)
	}
	sha := got.HeadSHA
	log := gitOut(t, repo, "log", "-1", "--format=%an <%ae>%n%B")
	if !strings.Contains(log, "Goblins <goblins@example.invalid>") || !strings.Contains(log, "Signed-off-by: Goblins") {
		t.Fatalf("attribution = %s", log)
	}

	// A later round on the same branch stays on it.
	w.run(workspace.PublishSpec("publish", "img", "ws", nil, w.access, "main", "goblins/t1", sha))
	if gitOut(t, w.remote, "rev-parse", "refs/heads/goblins/t1") != sha {
		t.Fatal("not pushed")
	}
	if gitOut(t, w.remote, "rev-parse", "refs/heads/main") != w.base {
		t.Fatal("base branch moved")
	}
	if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("feature v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := w.run(workspace.CommitSpec("commit", "img", "ws", nil, id, "goblins/t1", "Fix tests"))
	if !second.Changed || second.HeadSHA == sha {
		t.Fatalf("second commit = %+v", second)
	}
	w.run(workspace.PublishSpec("publish", "img", "ws", nil, w.access, "main", "goblins/t1", second.HeadSHA))
	if gitOut(t, w.remote, "rev-parse", "refs/heads/goblins/t1") != second.HeadSHA {
		t.Fatal("second round not pushed")
	}
}

func TestSymlinksAreNotCheckedOut(t *testing.T) {
	w := setup(t)
	w.prepare("ws", "goblins/t1")
	link := filepath.Join(w.rt.VolumePath("ws"), "repo", "evil-link")
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("a repository symlink exists in the agent's workspace")
	}
	// The planted symlink is unchanged from Git's view, so it does not appear as a change.
	got := w.run(workspace.CommitSpec("commit", "img", "ws", nil, id, "goblins/t1", "m"))
	if got.Changed {
		t.Fatal("symlink handling produced a spurious change")
	}
}

func TestOnlyPrepareAndPublishHoldCredentials(t *testing.T) {
	w := setup(t)
	check := func(spec core.WorkloadSpec, err error, wantSecret bool) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if got := len(spec.Secrets) > 0; got != wantSecret {
			t.Errorf("%s: secrets = %v", spec.Name, spec.Secrets)
		}
		if strings.Contains(strings.Join(spec.Command, " "), "s3cret-token") || strings.Contains(spec.Env["GOBLINS_REMOTE"], "s3cret-token") {
			t.Errorf("%s: token in command or URL", spec.Name)
		}
		if !wantSecret && spec.Network != core.NetworkNone {
			t.Errorf("%s: credential-free step has network", spec.Name)
		}
	}
	s, _ := workspace.PrepareSpec("prepare", "i", "v", nil, w.access, "main", w.base, "goblins/t")
	check(s, nil, true)
	s, _ = workspace.PublishSpec("publish", "i", "v", nil, w.access, "main", "goblins/t", w.base)
	check(s, nil, true)
	if !s.Mounts[0].ReadOnly {
		t.Error("publish must see the workspace read-only")
	}
	s, _ = workspace.CommitSpec("commit", "i", "v", nil, id, "goblins/t", "m")
	check(s, nil, false)
	s, _ = workspace.CopySpec("copy", "i", "v", "scratch", nil, w.base)
	check(s, nil, false)
	if !s.Mounts[0].ReadOnly || s.Mounts[1].ReadOnly {
		t.Errorf("copy mounts = %+v", s.Mounts)
	}
	check(workspace.InspectSpec("inspect", "i", "v", nil), nil, false)
}

// A workspace is assumed hostile. Nothing written into it may run while a token exists,
// which is the property that protects SCM credentials.
func TestHostileWorkspaceCannotReachPublishToken(t *testing.T) {
	w := setup(t)
	w.prepare("ws", "goblins/t1")
	vol := w.rt.VolumePath("ws")
	repo := filepath.Join(vol, "repo")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := w.run(workspace.CommitSpec("commit", "img", "ws", nil, id, "goblins/t1", "m"))

	canary := filepath.Join(t.TempDir(), "canary")
	payload := "#!/bin/sh\nenv > " + canary + "\n"
	for _, hook := range []string{"pre-push", "post-checkout", "pre-commit", "reference-transaction", "post-update"} {
		if err := os.WriteFile(filepath.Join(repo, ".git", "hooks", hook), []byte(payload), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	evil := filepath.Join(t.TempDir(), "evil.sh")
	if err := os.WriteFile(evil, []byte(payload), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := "\n[core]\n\tfsmonitor = " + evil + "\n\tsshCommand = " + evil + "\n\tpager = " + evil + "\n\talternateRefsCommand = " + evil +
		"\n[uploadpack]\n\tpackObjectsHook = " + evil + "\n[credential]\n\thelper = !" + evil + "\n[alias]\n\tpush = !" + evil + "\n"
	f, err := os.OpenFile(filepath.Join(repo, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(cfg); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	w.run(workspace.PublishSpec("publish", "img", "ws", nil, w.access, "main", "goblins/t1", got.HeadSHA))
	if _, err := os.Stat(canary); err == nil {
		out, _ := os.ReadFile(canary)
		t.Fatalf("hostile code ran during publish:\n%s", out)
	}
	if gitOut(t, w.remote, "rev-parse", "refs/heads/goblins/t1") != got.HeadSHA {
		t.Fatal("publish did not push")
	}
	if rcfg := gitOut(t, w.remote, "config", "--list"); strings.Contains(rcfg, evil) {
		t.Fatal("hostile configuration reached the remote")
	}
}

func TestForgedBundleIsRefused(t *testing.T) {
	w := setup(t)
	w.prepare("ws", "goblins/t1")
	vol := w.rt.VolumePath("ws")
	repo := filepath.Join(vol, "repo")
	if err := os.WriteFile(filepath.Join(repo, "good.txt"), []byte("validated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	validated := w.run(workspace.CommitSpec("commit", "img", "ws", nil, id, "goblins/t1", "validated"))

	// Later, the bundle named after the validated commit is replaced by other content.
	if err := os.WriteFile(filepath.Join(repo, "bad.txt"), []byte("backdoor\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := w.run(workspace.CommitSpec("commit", "img", "ws", nil, id, "goblins/t1", "tampered"))
	out := filepath.Join(vol, "out")
	if err := os.Rename(filepath.Join(out, bad.HeadSHA+".bundle"), filepath.Join(out, validated.HeadSHA+".bundle")); err != nil {
		t.Fatal(err)
	}

	spec, err := workspace.PublishSpec("publish-forged", "img", "ws", nil, w.access, "main", "goblins/t1", validated.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	res, err := runtime.RunToCompletion(ctx, w.rt, spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status.Phase == core.WorkloadSucceeded {
		t.Fatal("a forged bundle was pushed")
	}
	if err := workspace.ClassifyPush(res.Stderr); !core.IsKind(err, core.KindForbidden) {
		t.Fatalf("classification: %v (%s)", err, res.Stderr)
	}
	if out := exec.Command("git", "-C", w.remote, "rev-parse", "--verify", "-q", "refs/heads/goblins/t1"); out.Run() == nil {
		t.Fatal("something was pushed")
	}
}

func TestValidationCopyIsIsolated(t *testing.T) {
	w := setup(t)
	w.prepare("ws", "goblins/t1")
	repo := filepath.Join(w.rt.VolumePath("ws"), "repo")
	if err := os.WriteFile(filepath.Join(repo, "app.txt"), []byte("code\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := w.run(workspace.CommitSpec("commit", "img", "ws", nil, id, "goblins/t1", "m"))
	if err := os.WriteFile(filepath.Join(repo, "uncommitted.txt"), []byte("not part of the commit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := w.run(workspace.CopySpec("copy", "img", "ws", "scratch", nil, got.HeadSHA)); res.HeadSHA != got.HeadSHA {
		t.Fatalf("copy = %+v", res)
	}
	scratchRepo := filepath.Join(w.rt.VolumePath("scratch"), "repo")
	if b, err := os.ReadFile(filepath.Join(scratchRepo, "app.txt")); err != nil || string(b) != "code\n" {
		t.Fatalf("app.txt = %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(scratchRepo, "uncommitted.txt")); err == nil {
		t.Fatal("uncommitted file copied")
	}
	// Test code mutating its copy leaves the agent's workspace alone.
	if err := os.WriteFile(filepath.Join(scratchRepo, "app.txt"), []byte("tampered by tests\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "app.txt")); string(b) != "code\n" {
		t.Fatal("test run reached the workspace")
	}
}

func TestDivergedRemoteIsNotForcePushed(t *testing.T) {
	w := setup(t)
	w.prepare("ws", "goblins/t1")
	repo := filepath.Join(w.rt.VolumePath("ws"), "repo")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := w.run(workspace.CommitSpec("commit", "img", "ws", nil, id, "goblins/t1", "m"))
	// Someone else pushed to the branch meanwhile.
	other := t.TempDir()
	gitOut(t, other, "clone", "-q", w.remote, "c")
	clone := filepath.Join(other, "c")
	gitOut(t, clone, "checkout", "-q", "-b", "goblins/t1")
	if err := os.WriteFile(filepath.Join(clone, "b.txt"), []byte("b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitOut(t, clone, "add", "-A")
	gitOut(t, clone, "commit", "-qm", "human work")
	gitOut(t, clone, "push", "-q", "origin", "goblins/t1")
	human := gitOut(t, clone, "rev-parse", "HEAD")

	spec, err := workspace.PublishSpec("publish-div", "img", "ws", nil, w.access, "main", "goblins/t1", got.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	res, err := runtime.RunToCompletion(ctx, w.rt, spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status.Phase == core.WorkloadSucceeded {
		t.Fatal("non-fast-forward push succeeded")
	}
	if err := workspace.ClassifyPush(res.Stderr); !core.IsKind(err, core.KindConflict) {
		t.Fatalf("classification: %v (%s)", err, res.Stderr)
	}
	if gitOut(t, w.remote, "rev-parse", "refs/heads/goblins/t1") != human {
		t.Fatal("human work was overwritten")
	}
}

func TestPublishRefusesProtectedTargets(t *testing.T) {
	a := core.GitAccess{RemoteURL: "file:///x"}
	sha := strings.Repeat("a", 40)
	for _, branch := range []string{"main", "release", "feature/x"} {
		if _, err := workspace.PublishSpec("p", "i", "v", nil, a, "main", branch, sha); !core.IsKind(err, core.KindForbidden) && !core.IsKind(err, core.KindInvalid) {
			t.Errorf("%s accepted: %v", branch, err)
		}
	}
	for _, bad := range []string{"-rf", "a..b", "x.lock", "a b", "$(evil)", ""} {
		if _, err := workspace.PrepareSpec("p", "i", "v", nil, a, "main", sha, bad); !core.IsKind(err, core.KindInvalid) {
			t.Errorf("branch %q accepted: %v", bad, err)
		}
	}
	if _, err := workspace.PublishSpec("p", "i", "v", nil, a, "main", "goblins/t", "; rm -rf /"); !core.IsKind(err, core.KindInvalid) {
		t.Errorf("bad sha: %v", err)
	}
	if got := workspace.BranchName("t-1"); got != "goblins/t-1" {
		t.Fatal(got)
	}
}

func TestParseResult(t *testing.T) {
	r, err := workspace.ParseResult("noise\nGOBLINS_RESULT {\"head_sha\":\"forged\"}\nmore\nGOBLINS_RESULT {\"head_sha\":\"real\"}\n")
	if err != nil || r.HeadSHA != "real" {
		t.Fatalf("%+v, %v", r, err)
	}
	if _, err := workspace.ParseResult("nothing"); !core.IsKind(err, core.KindTransient) {
		t.Fatal(err)
	}
	if _, err := workspace.ParseResult("GOBLINS_RESULT not-json"); !core.IsKind(err, core.KindTransient) {
		t.Fatal(err)
	}
}

func TestReviewCheckoutIsPinnedAndTheDiffNeedsNoToken(t *testing.T) {
	w := setup(t)
	seed := filepath.Join(t.TempDir(), "seed")
	gitOut(t, filepath.Dir(seed), "clone", "-q", w.remote, seed)
	gitOut(t, seed, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(seed, "new.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitOut(t, seed, "add", "-A")
	gitOut(t, seed, "commit", "-qm", "first")
	pinned := gitOut(t, seed, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(seed, "later.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitOut(t, seed, "add", "-A")
	gitOut(t, seed, "commit", "-qm", "second")
	gitOut(t, seed, "push", "-q", w.remote, "feature")

	spec, err := workspace.ReviewPrepareSpec("fetch", "img", "rv", nil, w.access, workspace.ReviewTarget{BaseBranch: "main", HeadBranch: "feature", FetchRef: "refs/pull/1/head", SHA: pinned})
	if err != nil {
		t.Fatal(err)
	}
	res, err := runtime.RunToCompletion(ctx, w.rt, spec)
	if err != nil || res.Status.Phase != core.WorkloadSucceeded {
		t.Fatalf("fetch: %+v, %v", res, err)
	}
	parsed, err := workspace.ParseResult(res.Stdout)
	if err != nil || parsed.HeadSHA != pinned || parsed.BaseSHA != w.base {
		t.Fatalf("result = %+v, %v", parsed, err)
	}
	repo := filepath.Join(w.rt.VolumePath("rv"), "repo")
	if gitOut(t, repo, "rev-parse", "HEAD") != pinned {
		t.Fatal("not at the pinned commit")
	}
	if _, err := os.Stat(filepath.Join(repo, "later.txt")); err == nil {
		t.Fatal("the checkout has a newer commit's files")
	}
	diff, err := workspace.DiffSpec("diff", "img", "rv", nil, pinned, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if diff.Network != core.NetworkNone || len(diff.Secrets) != 0 || !diff.Mounts[0].ReadOnly {
		t.Fatalf("diff spec = %+v", diff)
	}
	out, err := runtime.RunToCompletion(ctx, w.rt, diff)
	if err != nil || !strings.Contains(out.Stdout, "+++ b/new.txt") || strings.Contains(out.Stdout, "later.txt") {
		t.Fatalf("diff = %+v, %v", out, err)
	}
	for _, bad := range []func() error{
		func() error {
			_, e := workspace.ReviewPrepareSpec("x", "i", "v", nil, w.access, workspace.ReviewTarget{BaseBranch: "main", HeadBranch: "feature", FetchRef: "not/a-ref; rm -rf /", SHA: pinned})
			return e
		},
		func() error {
			_, e := workspace.ReviewPrepareSpec("x", "i", "v", nil, w.access, workspace.ReviewTarget{BaseBranch: "main", HeadBranch: "feature", SHA: "HEAD"})
			return e
		},
		func() error { _, e := workspace.DiffSpec("x", "i", "v", nil, "HEAD", 1); return e },
	} {
		if !core.IsKind(bad(), core.KindInvalid) {
			t.Fatal("bad input accepted")
		}
	}
}
