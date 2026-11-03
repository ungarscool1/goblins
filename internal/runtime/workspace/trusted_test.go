package workspace_test

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers/command"
	"github.com/ungarscool1/goblins/internal/runtime/workspace"
)

const baseManifest = `version: 1
checks:
  - name: conformance
    run: "sh conformance/check.sh > report.txt"
    artifacts: [report.txt]
`

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// seedChecks gives the base branch a manifest and a conformance test for a toy app.
func (w *world) seedChecks() {
	w.t.Helper()
	dir := filepath.Join(w.t.TempDir(), "c")
	gitOut(w.t, filepath.Dir(dir), "clone", "-q", w.remote, dir)
	write(w.t, dir, map[string]string{
		".goblins/validation.yaml": baseManifest,
		"conformance/check.sh": "grep -q good app.txt || " +
			"{ echo 'app.txt is not good'; exit 1; }\n",
	})
	gitOut(w.t, dir, "add", "-A")
	gitOut(w.t, dir, "commit", "-qm", "checks")
	gitOut(w.t, dir, "push", "-q", "origin", "main")
	w.base = gitOut(w.t, dir, "rev-parse", "HEAD")
}

// check runs what a change's commit gets: a copy, the trusted step, then the manifest's checks.
func (w *world) check(sha string, paths ...string) (core.ValidationReport, string) {
	w.t.Helper()
	w.run(workspace.CopySpec("copy", "img", "ws", "scratch", nil, sha))
	got := w.run(workspace.TrustedSpec("trusted", "img", "scratch", nil, w.base,
		".goblins/validation.yaml", paths))
	manifest, err := base64.StdEncoding.DecodeString(got.Manifest)
	if err != nil {
		w.t.Fatal(err)
	}
	changed, err := base64.StdEncoding.DecodeString(got.TrustedChanged)
	if err != nil {
		w.t.Fatal(err)
	}
	w.n++
	p := command.New("v", command.Options{Image: "img"})
	rep, err := p.Validate(ctx, core.ValidationRequest{
		WorkloadName: "check-" + string(rune('a'+w.n)), Runtime: w.rt, Manifest: manifest,
		Workspace: core.Mount{Volume: "scratch", Target: workspace.ScratchMount},
		WorkDir:   workspace.ScratchMount + "/repo",
	})
	if err != nil {
		w.t.Fatal(err)
	}
	return rep, string(changed)
}

func (w *world) change(files map[string]string, message string) string {
	w.t.Helper()
	write(w.t, filepath.Join(w.rt.VolumePath("ws"), "repo"), files)
	spec, err := workspace.CommitSpec("commit", "img", "ws", nil, id, "goblins/t1", message)
	return w.run(spec, err).HeadSHA
}

func TestAChangeCannotPassByWeakeningItsChecks(t *testing.T) {
	w := setup(t)
	w.seedChecks()
	w.prepare("ws", "goblins/t1")
	trusted := []string{"conformance"}

	// A bad change that also guts the test, drops in a helper and rewrites the manifest.
	bad := w.change(map[string]string{
		"app.txt":                  "bad\n",
		"conformance/check.sh":     "exit 0\n",
		"conformance/extra.sh":     "exit 0\n",
		".goblins/validation.yaml": "version: 1\nchecks:\n  - {name: nothing, run: \"true\"}\n",
	}, "bad")
	rep, changed := w.check(bad, trusted...)
	if rep.Status != core.ValidationFailed || len(rep.Checks) != 1 ||
		rep.Checks[0].Name != "conformance" {
		t.Fatalf("the bypass worked: %+v", rep)
	}
	want := "--- artifact: report.txt ---\napp.txt is not good"
	if !strings.Contains(rep.Checks[0].Output, want) {
		t.Fatalf("no actionable artifact:\n%s", rep.Checks[0].Output)
	}
	if !strings.Contains(changed, "conformance/check.sh") ||
		!strings.Contains(changed, "conformance/extra.sh") {
		t.Fatalf("changed = %q", changed)
	}
	scratch := filepath.Join(w.rt.VolumePath("scratch"), "repo", "conformance")
	if _, err := os.Stat(filepath.Join(scratch, "extra.sh")); !os.IsNotExist(err) {
		t.Fatalf("what the change added stayed: %v", err)
	}

	// A good change passes, still under the base commit's test.
	good := w.change(map[string]string{"app.txt": "good\n"}, "good")
	rep, _ = w.check(good, trusted...)
	if rep.Status != core.ValidationPassed {
		t.Fatalf("report = %+v", rep)
	}
}

func TestTrustedStepNeedsABaseCommit(t *testing.T) {
	_, err := workspace.TrustedSpec("t", "i", "s", nil, "", "", nil)
	if !core.IsKind(err, core.KindInvalid) {
		t.Fatalf("err = %v", err)
	}
}
