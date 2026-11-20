package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

// What a project decided before reaches the planner as cited background, from Goblins' own
// accepted memory and from the project's documents, and what a task did comes back as a
// proposal that no agent is told about until a person accepts it.
func TestMemoryReachesThePlannerAndProposalsWaitForAPerson(t *testing.T) {
	e := setup(t)
	docs := filepath.Join(e.root, "knowledge")
	if err := os.MkdirAll(docs, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docs, "feature-flags.md"), []byte("# Feature flags\n\n"+
		"Every feature is shipped behind a flag in flags.yaml."), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := strings.Replace(string(e.cfgYAML), "validations:\n", fmt.Sprintf("knowledge:\n"+
		"  docs: {type: files, options: {path: %s}}\nvalidations:\n", docs), 1)
	cfg = strings.Replace(cfg, "    validation: checks\n", "    validation: checks\n"+
		"    memory: {enabled: true, sources: [docs]}\n", 1)
	e.cfgYAML = []byte(cfg)
	e.boot()
	ctx := context.Background()
	now := time.Now()
	for _, m := range []core.MemoryRecord{
		{ID: "m-accepted", Namespace: "e2e/app", Kind: core.MemoryDecision,
			Title:  "The feature is configured in YAML",
			Body:   "Feature settings live in settings.yaml.",
			Status: core.MemoryAccepted, ApprovedBy: "lead", UpdatedAt: now},
		{ID: "m-guess", Namespace: "e2e/app", Kind: core.MemoryFix, Title: "Feature guess",
			Body: "A model thinks features need no tests.", Status: core.MemoryProposed,
			UpdatedAt: now},
		{ID: "m-other", Namespace: "other/app", Kind: core.MemoryDecision, Title: "Feature at home",
			Body: "Another installation's feature notes.", Status: core.MemoryAccepted,
			UpdatedAt: now},
	} {
		if err := e.store.PutMemory(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	e.toDesign()
	prompts := e.prompts()
	for _, want := range []string{"[memory:m-accepted]", "accepted by lead",
		"[docs:feature-flags.md]",
		"Every feature is shipped behind a flag"} {
		if !strings.Contains(prompts, want) {
			t.Errorf("the planner was not given %q", want)
		}
	}
	for _, unwanted := range []string{"m-guess", "features need no tests", "m-other",
		"Another installation"} {
		if strings.Contains(prompts, unwanted) {
			t.Errorf("the planner was given %q", unwanted)
		}
	}
	task := e.task()
	e.comment(maintainer, fmt.Sprintf("/approve-design %d", task.DesignRevision))
	e.settle()
	e.boot()
	e.settle()
	e.wantState(core.StateCompleted)
	all, err := e.store.ListMemories(ctx, []string{"e2e/app"})
	if err != nil {
		t.Fatal(err)
	}
	proposed := 0
	for _, m := range all {
		if m.CreatedBy == "goblins" {
			proposed++
			if m.Status != core.MemoryProposed || !strings.Contains(strings.Join(m.Sources, " "),
				"commit:") {
				t.Errorf("proposal = %+v", m)
			}
		}
	}
	if proposed == 0 {
		t.Fatal("the published change proposed no memory")
	}
}
