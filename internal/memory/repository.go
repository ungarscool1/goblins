package memory

import (
	"context"
	"path"
	"slices"
	"strings"
	"sync"

	"github.com/ungarscool1/goblins/internal/core"
)

// Bounds on what one search of a repository reads.
const (
	maxRepoFiles      = 500
	maxRepoFileBytes  = 256 << 10
	maxRepoFetches    = 100
	maxRepoCacheBytes = 16 << 20
)

// DefaultRepositoryDocs is what a repository source reads when its project names nothing.
var DefaultRepositoryDocs = []string{"README.md", "docs/**/*.md"}

// Repository is a project's own documentation, read through its SCM at the commit the task
// works from. Files are fetched once by content: a later run reads only what changed.
type Repository struct {
	Reader core.RepositoryReader
	Repo   core.RepoRef
	Ref    string
	Globs  []string
	Cache  *BlobCache
}

// Info implements core.Provider.
func (r Repository) Info() core.ProviderInfo {
	return core.ProviderInfo{Kind: core.ProviderKnowledge, Type: "repository", Name: "repo",
		ContractVersion: core.ContractVersion}
}

// Search implements core.KnowledgeSource.
func (r Repository) Search(ctx context.Context, q core.KnowledgeQuery) ([]core.KnowledgeDoc,
	error) {
	tree, err := r.Reader.Tree(ctx, r.Repo, r.Ref)
	if err != nil {
		return nil, err
	}
	globs := r.Globs
	if len(globs) == 0 {
		globs = DefaultRepositoryDocs
	}
	type scored struct {
		doc   core.KnowledgeDoc
		score int
	}
	var found []scored
	seen, fetched := 0, 0
	for _, f := range tree.Files {
		if !MatchGlob(globs, f.Path) || f.Size > maxRepoFileBytes {
			continue
		}
		if seen++; seen > maxRepoFiles {
			break
		}
		body, ok := r.Cache.get(f.ID)
		if !ok {
			if fetched++; fetched > maxRepoFetches {
				continue // the rest is read by later runs, from the cache
			}
			raw, err := r.Reader.Blob(ctx, r.Repo, f.ID)
			if err != nil {
				return nil, err
			}
			body = strings.ToValidUTF8(string(raw), "")
			r.Cache.put(f.ID, body)
		}
		title := TitleOf(f.Path, body)
		if s := Score(title, body, q.Terms); s > 0 {
			found = append(found, scored{core.KnowledgeDoc{ID: f.Path, Title: title, Body: body,
				Revision: r.Ref}, s})
		}
	}
	slices.SortStableFunc(found, func(a, b scored) int { return b.score - a.score })
	out := make([]core.KnowledgeDoc, 0, min(len(found), max(q.Limit, 1)))
	for _, s := range found[:min(len(found), max(q.Limit, 1))] {
		out = append(out, s.doc)
	}
	return out, nil
}

// MatchGlob says whether a slash-separated path matches one of the patterns. A pattern
// starting with **/ matches the file name at any depth below what precedes it.
func MatchGlob(globs []string, p string) bool {
	for _, g := range globs {
		dir, rest, deep := strings.Cut(g, "**/")
		if deep && strings.HasPrefix(p, dir) {
			if m, _ := path.Match(rest, path.Base(p)); m {
				return true
			}
		}
		if m, _ := path.Match(g, p); m {
			return true
		}
	}
	return false
}

// TitleOf is a document's first heading, or its file name.
func TitleOf(p, body string) string {
	for _, l := range strings.SplitN(body, "\n", 40) {
		if t, ok := strings.CutPrefix(strings.TrimSpace(l), "# "); ok {
			return strings.TrimSpace(t)
		}
	}
	return strings.TrimSuffix(path.Base(p), path.Ext(p))
}

// BlobCache keeps file contents by their ID, which names the content, so an entry never goes
// stale. It is bounded, and forgets the oldest entries first.
type BlobCache struct {
	mu    sync.Mutex
	items map[string]string
	order []string
	size  int
}

func (c *BlobCache) get(id string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.items[id]
	return v, ok
}

func (c *BlobCache) put(id, body string) {
	if c == nil || len(body) > maxRepoCacheBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = map[string]string{}
	}
	if _, ok := c.items[id]; ok {
		return
	}
	for c.size+len(body) > maxRepoCacheBytes && len(c.order) > 0 {
		old := c.order[0]
		c.order = c.order[1:]
		c.size -= len(c.items[old])
		delete(c.items, old)
	}
	c.items[id] = body
	c.order = append(c.order, id)
	c.size += len(body)
}
