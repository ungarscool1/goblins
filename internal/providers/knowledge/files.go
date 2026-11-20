// Package knowledge reads documentation people keep elsewhere, for agents to be told about:
// a folder of Markdown, Outline, Confluence. Every source here only reads, within the scopes
// it is configured for, and returns plain text as data.
package knowledge

import (
	"context"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/memory"
)

const (
	maxFileBytes = 256 << 10
	maxFiles     = 5000
)

// Files reads Markdown under one directory. It opens the directory as an os.Root, so neither
// a symbolic link nor a path with .. can reach a file outside it.
type Files struct {
	name  string
	dir   string
	globs []string
}

// NewFiles returns a source for a directory.
func NewFiles(name, dir string, globs []string) (*Files, error) {
	if dir == "" || !path.IsAbs(dir) {
		return nil, core.Errorf(core.KindInvalid, "knowledge.files", "path must be absolute")
	}
	if len(globs) == 0 {
		globs = []string{"*.md", "**/*.md"}
	}
	return &Files{name: name, dir: dir, globs: globs}, nil
}

// Info implements core.Provider.
func (f *Files) Info() core.ProviderInfo { return info(f.name, "files") }

// Search implements core.KnowledgeSource.
func (f *Files) Search(_ context.Context, q core.KnowledgeQuery) ([]core.KnowledgeDoc, error) {
	root, err := os.OpenRoot(f.dir)
	if err != nil {
		return nil, core.Errorf(core.KindTransient, "knowledge.files", "cannot open the directory")
	}
	defer func() { _ = root.Close() }()
	type scored struct {
		doc   core.KnowledgeDoc
		score int
	}
	var found []scored
	n := 0
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !f.matches(p) {
			return nil //nolint:nilerr // an unreadable entry is skipped, not fatal
		}
		if n++; n > maxFiles {
			return fs.SkipAll
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxFileBytes {
			return nil //nolint:nilerr // links, devices and huge files are not documents
		}
		raw, err := fs.ReadFile(root.FS(), p)
		if err != nil {
			return nil //nolint:nilerr // skipped like any unreadable file
		}
		title := titleOf(p, string(raw))
		if s := memory.Score(title, string(raw), q.Terms); s > 0 {
			found = append(found, scored{core.KnowledgeDoc{ID: p, Title: title, Body: string(raw),
				Revision: info.ModTime().UTC().Format("20060102T150405Z"), URL: "file:" + p,
				UpdatedAt: info.ModTime()}, s})
		}
		return nil
	})
	if err != nil {
		return nil, core.Wrap(core.KindTransient, "knowledge.files", err)
	}
	slices.SortStableFunc(found, func(a, b scored) int { return b.score - a.score })
	out := make([]core.KnowledgeDoc, 0, min(len(found), q.Limit))
	for _, s := range found[:min(len(found), max(q.Limit, 1))] {
		out = append(out, s.doc)
	}
	return out, nil
}

func (f *Files) matches(p string) bool {
	for _, g := range f.globs {
		if rest, ok := strings.CutPrefix(g, "**/"); ok {
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

// titleOf is a document's first heading, or its file name.
func titleOf(p, body string) string {
	for _, l := range strings.SplitN(body, "\n", 40) {
		if t, ok := strings.CutPrefix(strings.TrimSpace(l), "# "); ok {
			return strings.TrimSpace(t)
		}
	}
	return strings.TrimSuffix(path.Base(p), path.Ext(p))
}

func info(name, typ string) core.ProviderInfo {
	return core.ProviderInfo{Kind: core.ProviderKnowledge, Type: typ, Name: name,
		ContractVersion: core.ContractVersion}
}
