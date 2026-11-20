package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ungarscool1/goblins/internal/core"
)

// envConfig adds a development environment whose state is a directory: provisioning creates
// it, deploying copies the commit's feature into it, and verifying checks it is fixed.
func envConfig(t *testing.T, e *env, targets string) string {
	t.Helper()
	root := filepath.Join(e.root, "dev-envs")
	dir := root + `/$GOBLINS_ENV_TARGET`
	cfg := strings.Replace(string(e.cfgYAML), "validations:\n", fmt.Sprintf(`environments:
  dev:
    type: command
    options:
      image: any
      network: none
      allow_destructive: true
      targets: %s
      steps:
        - {name: provision, phase: acquire, kind: mutate, idempotent: true, run: "mkdir -p %[2]s"}
        - {name: deploy, phase: deploy, kind: mutate, idempotent: true,
	run: "cp feature.txt %[2]s/deployed && echo $GOBLINS_SHA > %[2]s/sha"}
        - {name: smoke, phase: verify, kind: read, run: "grep -q fixed %[2]s/deployed"}
        - {name: logs, phase: evidence, kind: read, run: "echo deployed: $(cat %[2]s/deployed)"}
        - {name: reset, phase: release, kind: destructive, idempotent: true, run: "rm -rf %[2]s"}
validations:
`, targets, dir), 1)
	cfg = strings.Replace(cfg, `{name: feature-is-fixed, run: "grep -q fixed feature.txt"}`,
		`{name: feature-exists, run: "test -f feature.txt"}`, 1)
	cfg = strings.Replace(cfg, "    validation: checks\n", "    validation: checks\n"+
		"    environment: {name: dev}\n", 1)
	return cfg
}

func TestEnvironmentCatchesWhatChecksMissAndIsReleased(t *testing.T) {
	e := setup(t)
	e.cfgYAML = []byte(envConfig(t, e, "[]"))
	e.boot()
	e.toDesign()
	task := e.task()
	e.comment(maintainer, fmt.Sprintf("/approve-design %d", task.DesignRevision))
	e.settle()
	e.boot()
	e.settle()
	task = e.wantState(core.StateCompleted)
	if got := e.agentRuns(implementerTools); got != 2 {
		t.Fatalf("implementer rounds = %d", got)
	}
	// The failure seen in the environment, with its evidence, went back to the implementer.
	prompts := e.prompts()
	for _, want := range []string{"smoke on dev/goblins-", "deployed: buggy"} {
		if !strings.Contains(prompts, want) {
			t.Errorf("feedback lacks %q", want)
		}
	}
	if len(e.gh.PullRequests(repo)) != 1 {
		t.Fatal("no pull request")
	}
	if !strings.Contains(e.botComments(), "smoke on dev/") {
		t.Errorf("the report does not name the environment checks:\n%s", e.botComments())
	}
	entries, _ := os.ReadDir(filepath.Join(e.root, "dev-envs"))
	if len(entries) != 0 {
		t.Errorf("the environment was not released: %v", entries)
	}
	if leases, _ := e.store.ListLeases(context.Background(), "env:"); len(leases) != 0 {
		t.Errorf("leases left: %+v", leases)
	}
}
