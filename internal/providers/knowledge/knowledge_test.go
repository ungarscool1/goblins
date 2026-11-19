package knowledge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ungarscool1/goblins/internal/core"
)

func q(terms ...string) core.KnowledgeQuery { return core.KnowledgeQuery{Terms: terms, Limit: 5} }

// A folder source reads Markdown under its directory and nothing else, even through links.
func TestFilesStayInTheirDirectory(t *testing.T) {
	root := t.TempDir()
	docs := filepath.Join(root, "docs")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{filepath.Join(docs, "adr"), outside} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(docs, "adr", "0001-queues.md"),
		"# Use one queue per tenant\n\nTenants never share a queue.")
	write(filepath.Join(docs, "notes.txt"), "tenant queue notes, not Markdown")
	write(filepath.Join(outside, "secret.md"), "# Tenant queue secrets\n\nprivate")
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(docs,
		"link.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(docs, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	f, err := NewFiles("docs", docs, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.Search(context.Background(), q("tenant", "queue"))
	if err != nil || len(got) != 1 || got[0].Title != "Use one queue per tenant" ||
		got[0].ID != "adr/0001-queues.md" {
		t.Fatalf("got = %+v, %v", got, err)
	}
	if _, err := NewFiles("x", "relative/path", nil); err == nil {
		t.Fatal("a relative path was accepted")
	}
}

func TestOutlineReadsOnlyItsCollections(t *testing.T) {
	var queries []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ol-token" ||
			r.URL.Path != "/api/documents.search" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		queries = append(queries, in)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]any{"document": map[string]any{"id": "d1", "title": "Tenant queues",
				"text": "One queue per tenant.", "url": "/doc/tenant-queues", "collectionId": "eng",
				"updatedAt": "2026-11-01T10:00:00Z", "revision": 4}},
			map[string]any{"document": map[string]any{"id": "d2", "title": "HR",
				"text": "salaries", "collectionId": "hr"}}}})
	}))
	defer srv.Close()
	o, err := NewOutline("outline", srv.URL, core.NewSecret("ol-token"), []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := o.Search(context.Background(), q("tenant", `queue"; drop`, "queues"))
	if err != nil || len(got) != 1 || got[0].URL != srv.URL+"/doc/tenant-queues" ||
		got[0].Revision != "4 edits" {
		t.Fatalf("got = %+v, %v", got, err)
	}
	if queries[0]["collectionId"] != "eng" || queries[0]["query"] != "tenant queues" {
		t.Fatalf("query = %v", queries[0])
	}
	bad, _ := NewOutline("outline", srv.URL, core.NewSecret("wrong"), []string{"eng"})
	if _, err := bad.Search(context.Background(), q("tenant")); !core.IsKind(err,
		core.KindAuthRequired) {
		t.Fatalf("bad token: %v", err)
	}
}

func TestConfluenceReadsOnlyItsSpaces(t *testing.T) {
	var cql string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cql = r.URL.Query().Get("cql")
		_ = json.NewEncoder(w).Encode(map[string]any{"_links": map[string]any{"base": "https://c.test/wiki"},
			"results": []any{
				map[string]any{"id": "1", "title": "Queues", "space": map[string]any{"key": "ENG"},
					"version": map[string]any{"number": 7, "when": "2026-10-01T00:00:00Z"},
					"body":    map[string]any{"storage": map[string]any{"value": "<p>One <b>queue</b> per tenant &amp; more</p>"}},
					"_links":  map[string]any{"webui": "/spaces/ENG/pages/1"}},
				map[string]any{"id": "2", "title": "Payroll",
					"space": map[string]any{"key": "HR"}}}})
	}))
	defer srv.Close()
	c, err := NewConfluence("wiki", srv.URL, basic("me@x.test", "tok"), []string{"ENG"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Search(context.Background(), q("queue", `x" OR space = "HR`))
	if err != nil || len(got) != 1 || got[0].Body != "One queue per tenant & more" ||
		got[0].Revision != "v7" || got[0].URL != "https://c.test/wiki/spaces/ENG/pages/1" {
		t.Fatalf("got = %+v, %v", got, err)
	}
	if cql != `type = page AND space IN ("ENG") AND text ~ "queue"` {
		t.Fatalf("cql = %s", cql)
	}
	if _, err := NewConfluence("wiki", srv.URL, "x", []string{`ENG" OR 1=1`}); err == nil {
		t.Fatal("a bad space key was accepted")
	}
	if !strings.HasPrefix(basic("a", "b"), "Basic ") {
		t.Fatal("basic auth")
	}
}
