package fakegithub

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// routeComment serves one comment by its ID, as /issues/comments/{id} does.
func (s *Server) routeComment(w http.ResponseWriter, r *http.Request, st *repoState, id string) {
	var found *Comment
	for _, n := range sortedKeys(st.comments) {
		for _, c := range st.comments[n] {
			if strconv.FormatInt(c.ID, 10) == id {
				found = c
			}
		}
	}
	switch {
	case found == nil:
		fail(w, 404, "no comment")
	case r.Method == http.MethodGet:
		writeJSON(w, 200, found)
	case r.Method == http.MethodPatch:
		var body struct {
			Body string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		found.Body, found.UpdatedAt = body.Body, s.Now().UTC().Truncate(time.Second)
		s.edits++
		writeJSON(w, 200, found)
	default:
		fail(w, 404, "unknown endpoint")
	}
}

// Edits counts the comments changed in place.
func (s *Server) Edits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.edits
}
