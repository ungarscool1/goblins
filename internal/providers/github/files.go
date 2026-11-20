package github

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"

	"github.com/ungarscool1/goblins/internal/core"
)

// maxBlobBytes bounds one file read through the API.
const maxBlobBytes = 1 << 20

// Tree implements core.RepositoryReader, with the Git trees API at a commit or branch.
func (s *SCM) Tree(ctx context.Context, repo core.RepoRef, ref string) (core.RepoTree, error) {
	var res struct {
		SHA       string `json:"sha"`
		Truncated bool   `json:"truncated"`
		Tree      []struct {
			Path string `json:"path"`
			Mode string `json:"mode"`
			Type string `json:"type"`
			SHA  string `json:"sha"`
			Size int64  `json:"size"`
		} `json:"tree"`
	}
	path := "/repos/" + repo.Slug + "/git/trees/" + url.PathEscape(ref) + "?recursive=1"
	if err := s.c.repoCall(ctx, repo.Slug, http.MethodGet, path, nil, &res); err != nil {
		return core.RepoTree{}, err
	}
	out := core.RepoTree{ID: res.SHA, Truncated: res.Truncated}
	for _, e := range res.Tree {
		// Only regular files: 120000 is a link and 160000 a submodule.
		if e.Type == "blob" && (e.Mode == "100644" || e.Mode == "100755") {
			out.Files = append(out.Files, core.RepoFile{Path: e.Path, ID: e.SHA, Size: e.Size})
		}
	}
	return out, nil
}

// Blob implements core.RepositoryReader.
func (s *SCM) Blob(ctx context.Context, repo core.RepoRef, id string) ([]byte, error) {
	var res struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		Size     int64  `json:"size"`
	}
	path := "/repos/" + repo.Slug + "/git/blobs/" + url.PathEscape(id)
	if err := s.c.repoCall(ctx, repo.Slug, http.MethodGet, path, nil, &res); err != nil {
		return nil, err
	}
	if res.Size > maxBlobBytes {
		return nil, core.Errorf(core.KindInvalid, "github.blob", "the file is over %d bytes",
			maxBlobBytes)
	}
	if res.Encoding != "base64" {
		return []byte(res.Content), nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(res.Content, "\n", ""))
	if err != nil {
		return nil, core.Wrap(core.KindTransient, "github.blob", err)
	}
	return raw, nil
}
