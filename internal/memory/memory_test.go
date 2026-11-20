package memory_test

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/memory"
	"github.com/ungarscool1/goblins/internal/storage"
)

var now = time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)

type source struct {
	docs []core.KnowledgeDoc
	err  error
	got  []string
}

func (s *source) Info() core.ProviderInfo {
	return core.ProviderInfo{Kind: core.ProviderKnowledge, Name: "wiki"}
}

func (s *source) Search(_ context.Context, q core.KnowledgeQuery) ([]core.KnowledgeDoc, error) {
	s.got = q.Terms
	return s.docs, s.err
}

func store(t *testing.T, records ...core.MemoryRecord) *storage.Store {
	t.Helper()
	st, err := storage.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, m := range records {
		if err := st.PutMemory(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func rec(id, ns, title string, status core.MemoryStatus) core.MemoryRecord {
	return core.MemoryRecord{ID: id, Namespace: ns, Kind: core.MemoryDecision, Title: title,
		Body: "Billing uses Postgres advisory locks for invoices.", Status: status,
		ApprovedBy: "lead", UpdatedAt: now}
}

func TestOnlyAcceptedMemoriesOfThisProjectAreGiven(t *testing.T) {
	expired := rec("m4", "work/billing", "Invoice locking, old", core.MemoryAccepted)
	expired.ExpiresAt = now.Add(-time.Hour)
	st := store(t,
		rec("m1", "work/billing", "Invoice locking", core.MemoryAccepted),
		rec("m2", "home/billing", "Invoice locking at home", core.MemoryAccepted),
		rec("m3", "work/billing", "Invoice locking, a model's guess", core.MemoryProposed),
		expired,
		rec("m5", "work", "Invoices everywhere use locks", core.MemoryAccepted),
		rec("m6", "work/other", "Invoice locking elsewhere", core.MemoryAccepted))
	pack, err := memory.Build(context.Background(), memory.Request{
		Namespaces: []string{"work/billing", "work"}, Store: st,
		Text: "Invoices are generated twice: fix invoice locking", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	refs := pack.Refs()
	if !slices.Equal(refs, []string{"memory:m1", "memory:m5"}) {
		t.Fatalf("refs = %v", refs)
	}
	text := pack.Text()
	for _, want := range []string{"accepted by lead", "may be out of date",
		"Postgres advisory locks"} {
		if !strings.Contains(text, want) {
			t.Errorf("pack lacks %q:\n%s", want, text)
		}
	}
}

func TestSourcesAreCitedBudgetedAndFailSafely(t *testing.T) {
	wiki := &source{docs: []core.KnowledgeDoc{
		{ID: "1", Title: "Invoice service", Body: strings.Repeat("invoice ", 400), Revision: "v3",
			URL: "https://wiki.test/1", UpdatedAt: now},
		{ID: "2", Title: "Lunch menu", Body: "pasta"}}}
	pack, err := memory.Build(context.Background(), memory.Request{
		Sources: []memory.Source{{Name: "wiki", Source: wiki}}, Text: "invoice service locking",
		Budget: 1200, Now: now})
	if err != nil || !slices.Equal(pack.Refs(), []string{"wiki:1"}) ||
		!strings.Contains(pack.Text(), "revision v3, updated 2026-11-19, https://wiki.test/1") {
		t.Fatalf("pack = %+v, %v", pack, err)
	}
	if len(pack.Text()) > 2000 {
		t.Fatalf("pack is %d bytes", len(pack.Text()))
	}
	if !slices.Contains(wiki.got, "invoice") || slices.Contains(wiki.got, "the") {
		t.Fatalf("terms = %v", wiki.got)
	}
	down := &source{err: core.Errorf(core.KindTransient, "x", "down")}
	pack, err = memory.Build(context.Background(), memory.Request{
		Sources: []memory.Source{{Name: "wiki", Source: down}}, Text: "invoice", Now: now})
	if err != nil || len(pack.Warnings) != 1 {
		t.Fatalf("optional source down: %+v, %v", pack, err)
	}
	_, err = memory.Build(context.Background(), memory.Request{
		Sources: []memory.Source{{Name: "wiki", Source: down, Required: true}}, Text: "invoice",
		Now: now})
	if err == nil || !strings.Contains(err.Error(), "required knowledge source wiki") {
		t.Fatalf("required source down: %v", err)
	}
}

func TestTermsAreWordsOnly(t *testing.T) {
	got := memory.Terms(`Fix the "login" flow; DROP TABLE users;-- and <script> login again`)
	for _, w := range got {
		if strings.ContainsAny(w, `";<>- `) {
			t.Errorf("term %q", w)
		}
	}
	if got[0] != "login" {
		t.Fatalf("terms = %v", got)
	}
}
