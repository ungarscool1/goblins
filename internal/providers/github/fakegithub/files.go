package fakegithub

import (
	"crypto/sha1" //nolint:gosec // names content like Git does; nothing relies on it for security
	"encoding/base64"
	"encoding/hex"
	"maps"
	"net/http"
	"slices"
)

// SetTree makes the Git trees and blobs API return these files for a repository, at any ref.
func (s *Server) SetTree(repo string, files map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.trees == nil {
		s.trees = map[string]map[string]string{}
	}
	s.trees[repo] = maps.Clone(files)
}

func blobID(body string) string {
	sum := sha1.Sum([]byte(body)) //nolint:gosec // see the import
	return hex.EncodeToString(sum[:])
}

// routeGit answers GET git/trees/{ref} and git/blobs/{id}. It runs with the lock held.
func (s *Server) routeGit(w http.ResponseWriter, repo string, seg []string) {
	files := s.trees[repo]
	switch seg[1] {
	case "trees":
		var tree []map[string]any
		for _, p := range slices.Sorted(maps.Keys(files)) {
			tree = append(tree, map[string]any{"path": p, "mode": "100644", "type": "blob",
				"sha": blobID(files[p]), "size": len(files[p])})
		}
		writeJSON(w, 200, map[string]any{"sha": "tree-" + seg[2], "truncated": false,
			"tree": tree})
	case "blobs":
		for _, body := range files {
			if blobID(body) == seg[2] {
				writeJSON(w, 200, map[string]any{"sha": seg[2], "size": len(body),
					"encoding": "base64",
					"content":  base64.StdEncoding.EncodeToString([]byte(body))})
				return
			}
		}
		fail(w, 404, "Not Found")
	default:
		fail(w, 404, "unknown endpoint")
	}
}
