package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/storage"
)

func TestMemoryCuration(t *testing.T) {
	ctx := context.Background()
	st, err := storage.OpenSQLite(ctx, filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	run := func(cmd string, f memoryFlags, ns, in string) string {
		t.Helper()
		var out bytes.Buffer
		if err := runMemory(ctx, cmd, f, ns, st, strings.NewReader(in), &out); err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		return out.String()
	}
	id := strings.TrimSpace(run("add", memoryFlags{title: "Queues", body: "One queue per tenant.",
		as: "lead", kind: "decision"}, "work/ops", ""))
	if !strings.Contains(run("search", memoryFlags{text: "tenant queue"}, "work/ops", ""), id) {
		t.Fatal("an accepted memory was not found")
	}
	if run("list", memoryFlags{}, "home/ops", "") != "" {
		t.Fatal("another namespace sees the memory")
	}
	exported := run("export", memoryFlags{}, "work/ops", "")
	// An export imported elsewhere lands in that namespace, not the one it names.
	run("import", memoryFlags{}, "lab/ops", strings.ReplaceAll(exported, id, "copy"))
	if m, err := st.GetMemory(ctx, "copy"); err != nil || m.Namespace != "lab/ops" {
		t.Fatalf("imported = %+v, %v", m, err)
	}
	var out bytes.Buffer
	err = runMemory(ctx, "forget", memoryFlags{id: id}, "home/ops", st, nil, &out)
	if err == nil {
		t.Fatal("a memory was forgotten from another namespace")
	}
	run("forget", memoryFlags{id: id}, "work/ops", "")
	if _, err := st.GetMemory(ctx, id); !core.IsKind(err, core.KindNotFound) {
		t.Fatalf("after forget: %v", err)
	}
	proposal := core.MemoryRecord{ID: "p1", Namespace: "work/ops", Title: "Proposed",
		Status: core.MemoryProposed}
	_ = st.PutMemory(ctx, proposal)
	run("accept", memoryFlags{id: "p1", as: "lead"}, "work/ops", "")
	if m, _ := st.GetMemory(ctx, "p1"); m.Status != core.MemoryAccepted || m.ApprovedBy != "lead" {
		t.Fatalf("accepted = %+v", m)
	}
}

func TestMemoryShowAndSources(t *testing.T) {
	t.Setenv("GOBLINS_CONFIG", "")
	dir := t.TempDir()
	docs := filepath.Join(dir, "docs")
	if err := os.MkdirAll(docs, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docs, "flags.md"),
		[]byte("# Feature flags\nEvery feature ships behind a flag."), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "goblins.yaml")
	if err := os.WriteFile(cfg, []byte(`version: 1
installation: {name: test, data_dir: `+filepath.Join(dir, "data")+`}
trackers: {mock: {type: fake}}
scms: {mock: {type: fake}}
event_sources: {mock: {type: fake}}
agents: {mock: {type: fake}}
runtimes: {mock: {type: fake}}
knowledge:
  docs: {type: files, options: {path: `+docs+`}}
  gone: {type: files, options: {path: `+filepath.Join(dir, "missing")+`}}
projects:
  - id: demo
    tracker: mock
    scm: mock
    event_sources: [mock]
    repository: example/demo
    base_branch: main
    policy: {allow_unvalidated_draft_pr: true, allow_skipped_review: true}
    memory: {enabled: true, sources: [docs, gone], required: [docs]}
    stages:
      planner: {agent: mock, runtime: mock}
      implementer: {agent: mock, runtime: mock}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var out, errb bytes.Buffer
	err := run(ctx, []string{"memory", "sources", "--config", cfg, "--project", "demo",
		"--text", "feature flags"}, &out, &errb)
	got := out.String()
	if err == nil || !strings.Contains(got, "docs\tfiles\trequired\tok, 1 documents") ||
		!strings.Contains(got, "docs:flags.md\tFeature flags") ||
		!strings.Contains(got, "gone\tfiles\toptional\tfailed:") {
		t.Fatalf("sources: %v\n%s%s", err, got, errb.String())
	}

	out.Reset()
	if err := run(ctx, []string{"memory", "add", "--config", cfg, "--project", "demo",
		"--title", "Flags", "--body", "Ship behind a flag.", "--source", "docs:flags.md@r1",
		"--as", "lead"}, &out, &errb); err != nil {
		t.Fatal(err, errb.String())
	}
	id := strings.TrimSpace(out.String())
	out.Reset()
	if err := run(ctx, []string{"memory", "show", "--config", cfg, "--project", "demo",
		"--id", id}, &out, &errb); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{id + ": Flags", "accepted by lead", "source: docs:flags.md@r1",
		"Ship behind a flag."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("show lacks %q:\n%s", want, out.String())
		}
	}
	if err := run(ctx, []string{"memory", "show", "--config", cfg, "--id", id}, &out,
		&errb); err == nil {
		t.Error("a project's memory was shown from the installation's namespace")
	}
}
