//go:build docker

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ungarscool1/goblins/internal/api"
	"github.com/ungarscool1/goblins/internal/app"
	"github.com/ungarscool1/goblins/internal/config"
	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers"
	"github.com/ungarscool1/goblins/internal/providers/claude"
	"github.com/ungarscool1/goblins/internal/providers/command"
	gh "github.com/ungarscool1/goblins/internal/providers/github"
	"github.com/ungarscool1/goblins/internal/providers/github/fakegithub"
	rtpkg "github.com/ungarscool1/goblins/internal/runtime"
	"github.com/ungarscool1/goblins/internal/runtime/docker"
)

const e2eImage = "goblins-e2e:dev"

// withRemote gives Git steps access to the volume standing in for the Git server, the way
// an HTTPS remote would be reachable in production. Nothing else gets it.
type withRemote struct {
	core.ExecutionRuntime
	volume string
}

func (r withRemote) Create(ctx context.Context, spec core.WorkloadSpec) (core.WorkloadStatus, error) {
	if strings.Contains(spec.Name, "-prepare-") || strings.Contains(spec.Name, "-publish-") {
		spec.Mounts = append(spec.Mounts, core.Mount{Volume: r.volume, Target: "/remote"})
	}
	return r.ExecutionRuntime.Create(ctx, spec)
}

func dockerHost() string {
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		return h
	}
	if home, _ := os.UserHomeDir(); home != "" {
		if _, err := os.Stat(home + "/.docker/run/docker.sock"); err == nil {
			return "unix://" + home + "/.docker/run/docker.sock"
		}
	}
	return ""
}

// TestIssueToDraftPullRequestInContainers runs the MVP scenario with real containers. Every
// workload is confined as in production: non-root, no capabilities, read-only root.
func TestIssueToDraftPullRequestInContainers(t *testing.T) {
	stamp := fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000_000)
	network := "goblins-e2e-workers-" + stamp
	base, err := docker.New("docker", docker.Options{Host: dockerHost(), PullMissing: false, Network: network})
	if err != nil {
		t.Fatal(err)
	}
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := base.List(probe, map[string]string{"goblins.none": "x"}); err != nil {
		t.Skipf("no Docker engine: %v", err)
	}
	if out, err := exec.Command("docker", "build", "-q", "-t", e2eImage, "testdata").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	remote, agentVol := "goblins-e2e-remote-"+stamp, "goblins-e2e-config-"+stamp
	for _, v := range []string{remote, agentVol} {
		if err := base.EnsureVolume(ctx, v, nil); err != nil {
			t.Fatal(err)
		}
	}
	installation := "e2e" + stamp
	t.Cleanup(func() {
		for _, v := range []string{remote, agentVol} {
			_ = base.RemoveVolume(ctx, v)
		}
		_ = exec.Command("docker", "network", "rm", network).Run()
	})

	step := 0
	run := func(mount, script string) string {
		t.Helper()
		step++
		res, err := rtpkg.RunToCompletion(ctx, base, core.WorkloadSpec{Name: fmt.Sprintf("goblins-e2e-%s-%d", stamp, step), Image: e2eImage, Command: []string{"sh", "-c", script}, Network: core.NetworkNone,
			Env: map[string]string{"HOME": "/tmp"}, Mounts: []core.Mount{{Volume: mount, Target: "/scratch"}}})
		if err != nil {
			t.Fatal(err)
		}
		if res.Status.Phase != core.WorkloadSucceeded {
			t.Fatalf("helper failed: %s\n%s", res.Stdout, res.Stderr)
		}
		return strings.TrimSpace(res.Stdout)
	}
	// /scratch belongs to the worker user in the image, and an empty volume inherits that.
	seed := "set -eu; export GIT_AUTHOR_NAME=s GIT_AUTHOR_EMAIL=s@e GIT_COMMITTER_NAME=s GIT_COMMITTER_EMAIL=s@e; mkdir -p /scratch/acme; git init -q --bare -b main /scratch/acme/app.git; d=$(mktemp -d); cd $d; git init -q -b main; echo demo > README.md; git add -A; git commit -qm init; git push -q /scratch/acme/app.git main; git rev-parse HEAD"
	baseSHA := run(remote, seed)

	srv := fakegithub.New(t)
	srv.AddRepo(repo)
	srv.Grant(repo, maintainerID, "write")
	srv.BranchLookup = func(_, branch string) (string, bool) {
		out := run(remote, "git --git-dir=/scratch/acme/app.git rev-parse --verify -q refs/heads/"+branch+" || true")
		return out, out != ""
	}
	root := t.TempDir()
	keyFile := root + "/app.pem"
	if err := os.WriteFile(keyFile, srv.KeyPEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("E2E_WEBHOOK_SECRET", webhookSecret)
	cfgYAML := fmt.Appendf(nil, `
version: 1
installation: {name: %[1]s, data_dir: %[2]s/data}
trackers:
  forge: {type: github, options: {app_id: 42, private_key_file: %[3]s, api_url: %[4]s}}
scms:
  forge: {type: github, options: {app_id: 42, private_key_file: %[3]s, api_url: %[4]s, git_url: "file:///remote"}}
event_sources:
  forge: {type: github, options: {webhook_secret_env: E2E_WEBHOOK_SECRET, repositories: [%[5]s]}}
agents:
  claude: {type: claude-cli, options: {image: %[6]s, config_volume: %[7]s}}
runtimes:
  engine: {type: docker-e2e}
validations:
  checks:
    type: command
    options:
      image: %[6]s
      checks:
        - {name: feature-is-fixed, run: "grep -q fixed feature.txt"}
projects:
  - id: app
    tracker: forge
    scm: forge
    event_sources: [forge]
    repository: %[5]s
    validation: checks
    policy: {allow_skipped_review: true}
    workspace: {git_image: %[6]s}
    stages:
      planner: {agent: claude, runtime: engine}
      implementer: {agent: claude, runtime: engine}
`, installation, root, keyFile, srv.URL, repo, e2eImage, agentVol)

	reg := providers.NewRegistry()
	gh.Register(reg)
	claude.Register(reg)
	command.Register(reg)
	wrapped := withRemote{ExecutionRuntime: base, volume: remote}
	reg.Register(providers.KindRuntime, "docker-e2e", func(string, providers.Deps, providers.Options) (any, error) { return wrapped, nil })
	cfg, err := config.LoadBytes(cfgYAML, os.LookupEnv, reg.Known)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.Installation.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := app.OpenStore(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	sys, err := app.Build(cfg, store, discard, app.Options{Registry: reg})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("POST /webhooks/{source}", api.WebhookHandler(sys.Webhooks, sys.Inbox, discard))
	deliver := func(event string, payload map[string]any) {
		t.Helper()
		payload["repository"] = map[string]any{"full_name": repo}
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/webhooks/forge", strings.NewReader(string(body)))
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("X-Hub-Signature-256", fakegithub.Sign(webhookSecret, body))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("webhook: %d", rec.Code)
		}
	}
	settle := func() { t.Helper(); sys.Scheduler.Step(ctx) }
	issue := srv.AddIssue(repo, "Add the feature", "Please", maintainer)
	subject := core.Subject{Kind: core.SubjectIssue, Provider: "forge", Container: repo, ExternalID: fmt.Sprint(issue.Number)}
	want := func(s core.State) core.Task {
		t.Helper()
		task, err := store.FindTask(ctx, "app", subject)
		if err != nil {
			t.Fatal(err)
		}
		if task.State != s {
			t.Fatalf("state = %s (%q), want %s", task.State, task.Reason, s)
		}
		return task
	}
	say := func(body string) {
		t.Helper()
		c := srv.AddComment(repo, issue.Number, maintainer, body)
		deliver("issue_comment", map[string]any{"action": "created", "issue": srv.GetIssue(repo, issue.Number), "comment": c, "sender": maintainer})
		settle()
	}

	deliver("issues", map[string]any{"action": "opened", "issue": issue, "sender": maintainer})
	settle()
	want(core.StateWaitingForUser)
	say("Use Postgres please")
	want(core.StateDesignProposed)
	say("/approve-design 1")
	task := want(core.StateCompleted)

	head := run(remote, "git --git-dir=/scratch/acme/app.git rev-parse refs/heads/goblins/"+task.ID)
	if head != task.ValidatedSHA {
		t.Fatalf("pushed %s, validated %s", head, task.ValidatedSHA)
	}
	if got := run(remote, "git --git-dir=/scratch/acme/app.git rev-parse refs/heads/main"); got != baseSHA {
		t.Fatal("default branch moved")
	}
	if got := run(remote, "git --git-dir=/scratch/acme/app.git show "+head+":feature.txt"); got != "fixed" {
		t.Fatalf("feature.txt = %q", got)
	}
	prs := srv.PullRequests(repo)
	if len(prs) != 1 || !prs[0].Draft {
		t.Fatalf("pull requests = %+v", prs)
	}
	inv := run(agentVol, "cat /scratch/invocations")
	for _, wantStr := range []string{"WHO uid=10001 socket=no", "ENV API_KEY=[] AUTH_TOKEN=[] GITPW=[]", "--restricted"} {
		if !strings.Contains(inv, wantStr) {
			t.Errorf("agent workload lacks %q:\n%s", wantStr, inv)
		}
	}
	left, err := base.List(ctx, map[string]string{"goblins.installation": installation})
	if err != nil || len(left) != 0 {
		t.Fatalf("workloads left: %+v, %v", left, err)
	}
}
