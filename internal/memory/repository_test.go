package memory_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/memory"
)

// repoReader is a repository's tree at one commit, counting the files it was asked for.
type repoReader struct {
	files map[string]string
	reads map[string]int
}

func (r *repoReader) Tree(_ context.Context, _ core.RepoRef, ref string) (core.RepoTree, error) {
	t := core.RepoTree{ID: "tree-" + ref}
	for p, body := range r.files {
		t.Files = append(t.Files, core.RepoFile{Path: p, ID: "blob-" + p + "-" + body[:4],
			Size: int64(len(body))})
	}
	return t, nil
}

func (r *repoReader) Blob(_ context.Context, _ core.RepoRef, id string) ([]byte, error) {
	r.reads[id]++
	for p, body := range r.files {
		if id == "blob-"+p+"-"+body[:4] {
			return []byte(body), nil
		}
	}
	return nil, core.Errorf(core.KindNotFound, "test", "no blob %s", id)
}

func TestRepositoryDocsAreReadAtTheCommitOnce(t *testing.T) {
	r := &repoReader{reads: map[string]int{}, files: map[string]string{
		"README.md":              "# Billing\nInvoices are locked with advisory locks.",
		"docs/adr/0004-locks.md": "# Invoice locks\nWhy invoices use advisory locks.",
		"docs/other.md":          "# Unrelated\nNothing about it.",
		"src/locks.go":           "package locks // advisory invoices",
		"notes/invoice.md":       "# Not listed\nadvisory invoice notes",
	}}
	src := memory.Repository{Reader: r, Repo: core.RepoRef{Slug: "o/r"}, Ref: "abc123",
		Cache: &memory.BlobCache{}}
	q := core.KnowledgeQuery{Terms: []string{"invoice", "advisory"}, Limit: 5}
	docs, err := src.Search(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range docs {
		got = append(got, d.ID+"@"+d.Revision)
	}
	if strings.Join(got, " ") != "docs/adr/0004-locks.md@abc123 README.md@abc123" &&
		strings.Join(got, " ") != "README.md@abc123 docs/adr/0004-locks.md@abc123" {
		t.Fatalf("docs = %v", got)
	}
	if docs[0].Title == "" || strings.Contains(strings.Join(got, " "), "src/") {
		t.Fatalf("docs = %+v", docs)
	}
	// A second run reads nothing again: files are cached by content.
	if _, err := src.Search(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	for id, n := range r.reads {
		if n != 1 {
			t.Errorf("%s read %d times", id, n)
		}
	}
}

func TestMatchGlob(t *testing.T) {
	globs := []string{"README.md", "docs/**/*.md"}
	for p, want := range map[string]bool{
		"README.md": true, "docs/a.md": true, "docs/adr/b.md": true,
		"x/README.md": false, "docs.md": false, "src/docs/a.md": false, "docs/a.txt": false,
	} {
		if got := memory.MatchGlob(globs, p); got != want {
			t.Errorf("%s: %v, want %v", p, got, want)
		}
	}
}

// A memory citing a document at an older revision than the one found now is flagged, for the
// agent and in the run's record, and still given.
func TestAMemoryWhoseSourceChangedIsFlagged(t *testing.T) {
	m := rec("m1", "work/billing", "Invoice locking", core.MemoryAccepted)
	m.Sources = []string{"wiki:locks@r1"}
	src := &source{docs: []core.KnowledgeDoc{{ID: "locks", Title: "Invoice locks",
		Body: "Invoices now use row locks.", Revision: "r2"}}}
	pack, err := memory.Build(context.Background(), memory.Request{
		Namespaces: []string{"work/billing"}, Store: store(t, m),
		Sources: []memory.Source{{Name: "wiki", Source: src}},
		Text:    "invoice locking", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(pack.Warnings) != 1 || !strings.Contains(pack.Warnings[0], "wiki:locks at revision r1, now r2") {
		t.Fatalf("warnings = %v", pack.Warnings)
	}
	if !strings.Contains(pack.Text(), "CHANGED SOURCE") || len(pack.Items) != 2 {
		t.Fatalf("pack:\n%s", pack.Text())
	}
}
