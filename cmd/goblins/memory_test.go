package main

import (
	"bytes"
	"context"
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
