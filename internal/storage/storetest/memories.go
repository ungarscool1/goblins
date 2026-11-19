package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

// Memories runs the memory part of the conformance suite.
func Memories(t *testing.T, build func(t *testing.T) core.StateStore) {
	ctx := context.Background()
	t0 := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	s := build(t)
	work := core.MemoryRecord{ID: "m1", Namespace: "work/ops", Kind: core.MemoryDecision,
		Title: "Use Postgres", Body: "We use Postgres 17.", Status: core.MemoryAccepted,
		Tags: []string{"db"}, Sources: []string{"task:t1"}, UpdatedAt: t0}
	home := core.MemoryRecord{ID: "m2", Namespace: "home/app", Kind: core.MemoryFix,
		Title: "Private", Body: "secret plans", Status: core.MemoryAccepted, UpdatedAt: t0}
	for _, m := range []core.MemoryRecord{work, home} {
		if err := s.PutMemory(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListMemories(ctx, []string{"work/ops", "work"})
	if err != nil || len(got) != 1 || got[0].ID != "m1" || got[0].Tags[0] != "db" {
		t.Fatalf("list = %+v, %v", got, err)
	}
	work.Status, work.UpdatedAt = core.MemorySuperseded, t0.Add(time.Hour)
	if err := s.PutMemory(ctx, work); err != nil {
		t.Fatal(err)
	}
	if m, err := s.GetMemory(ctx, "m1"); err != nil || m.Status != core.MemorySuperseded {
		t.Fatalf("get = %+v, %v", m, err)
	}
	if err := s.DeleteMemory(ctx, "m1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetMemory(ctx, "m1"); !core.IsKind(err, core.KindNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := s.PutMemory(ctx, core.MemoryRecord{ID: "x"}); !core.IsKind(err, core.KindInvalid) {
		t.Fatalf("no namespace: %v", err)
	}
}
