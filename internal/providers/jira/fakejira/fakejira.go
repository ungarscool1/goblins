// Package fakejira is an in-memory Jira REST API version 2, in its Cloud or Data Center
// form, for tests. It enforces what the adapter must cope with: credentials, paging, rate
// limits and permissions.
package fakejira

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Time is Jira's timestamp layout.
const Time = "2006-01-02T15:04:05.000-0700"

// User is an account.
type User struct {
	AccountID   string `json:"accountId,omitempty"`
	AccountType string `json:"accountType,omitempty"`
	Key         string `json:"key,omitempty"`
	Name        string `json:"name,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
}

// Comment is a stored comment.
type Comment struct {
	ID         string           `json:"id"`
	Author     User             `json:"author"`
	Body       string           `json:"body"`
	Created    string           `json:"created"`
	Updated    string           `json:"updated"`
	Properties []map[string]any `json:"properties,omitempty"`
}

// History is a changelog entry.
type History struct {
	ID      string           `json:"id"`
	Author  User             `json:"author"`
	Created string           `json:"created"`
	Items   []map[string]any `json:"items"`
}

// Issue is a stored issue.
type Issue struct {
	ID, Key, Project, Summary, Description, Status, Category string
	Labels                                                   []string
	Reporter                                                 User
	Created, Updated                                         time.Time
	Comments                                                 []Comment
	Histories                                                []History
}

// Server is the fake. Set fields before use, under no lock; change state through methods.
type Server struct {
	*httptest.Server
	// Cloud selects the Cloud flavour: account IDs and token-paged search.
	Cloud bool
	// Token is the bearer token or Basic credential that is accepted.
	Authorization string
	Me            User
	// PageSize caps every page, to exercise paging.
	PageSize int

	mu        sync.Mutex
	issues    map[string]*Issue
	perms     map[string]map[string]bool // account -> permission
	limited   int
	expired   bool
	disabled  bool
	nextID    int
	now       func() time.Time
	Requests  []string
	Transited []string
}

// New starts a server.
func New(cloud bool, authorization string, me User, now func() time.Time) *Server {
	s := &Server{Cloud: cloud, Authorization: authorization, Me: me, PageSize: 100,
		issues: map[string]*Issue{}, perms: map[string]map[string]bool{}, now: now}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// ID is how the flavour identifies an account.
func (s *Server) ID(u User) string {
	if s.Cloud {
		return u.AccountID
	}
	return u.Name
}

// AddIssue stores an issue and returns it.
func (s *Server) AddIssue(project, summary, description string, reporter User) *Issue {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	n := 0
	for _, i := range s.issues {
		if i.Project == project {
			n++
		}
	}
	now := s.now()
	i := &Issue{ID: strconv.Itoa(10000 + s.nextID), Key: fmt.Sprintf("%s-%d", project, n+1),
		Project: project, Summary: summary, Description: description, Status: "To Do",
		Category: "new", Reporter: reporter, Created: now, Updated: now}
	s.issues[i.Key] = i
	return i
}

// AddComment adds a comment by someone.
func (s *Server) AddComment(key string, author User, body string) Comment {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addComment(s.issues[key], author, body, nil)
}

func (s *Server) addComment(i *Issue, author User, body string, props []map[string]any) Comment {
	s.nextID++
	now := s.now().Format(Time)
	c := Comment{ID: strconv.Itoa(s.nextID), Author: author, Body: body, Created: now,
		Updated: now, Properties: props}
	i.Comments = append(i.Comments, c)
	i.Updated = s.now()
	return c
}

// EditComment changes a comment's text later.
func (s *Server) EditComment(key, id, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.issues[key]
	for n := range i.Comments {
		if i.Comments[n].ID == id {
			i.Comments[n].Body = body
			i.Comments[n].Updated = s.now().Format(Time)
		}
	}
	i.Updated = s.now()
}

// Change records a changelog entry, applying label and status changes.
func (s *Server) Change(key string, author User, field, from, to string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.issues[key]
	s.nextID++
	i.Histories = append(i.Histories, History{ID: strconv.Itoa(s.nextID), Author: author,
		Created: s.now().Format(Time),
		Items:   []map[string]any{{"field": field, "fromString": from, "toString": to}}})
	switch field {
	case "labels":
		i.Labels = strings.Fields(to)
	case "status":
		i.Status, i.Category = to, "indeterminate"
		if to == "Done" {
			i.Category = "done"
		}
	case "description":
		i.Description = to
	case "summary":
		i.Summary = to
	}
	i.Updated = s.now()
}

// Grant gives an account a Jira permission, such as EDIT_ISSUES.
func (s *Server) Grant(id string, perms ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perms[id] == nil {
		s.perms[id] = map[string]bool{}
	}
	for _, p := range perms {
		s.perms[id][p] = true
	}
}

// ResetPermissions removes every permission of an account.
func (s *Server) ResetPermissions(id string) { s.mu.Lock(); delete(s.perms, id); s.mu.Unlock() }

// RateLimit answers the next n requests with 429.
func (s *Server) RateLimit(n int) { s.mu.Lock(); s.limited = n; s.mu.Unlock() }

// Expire refuses the credentials from now on.
func (s *Server) Expire() { s.mu.Lock(); s.expired = true; s.mu.Unlock() }

// DisableAPI refuses API access for these credentials, as a site policy can.
func (s *Server) DisableAPI() { s.mu.Lock(); s.disabled = true; s.mu.Unlock() }

// Issue returns a copy of an issue.
func (s *Server) Issue(key string) Issue {
	s.mu.Lock()
	defer s.mu.Unlock()
	return *s.issues[key]
}

var (
	issuePath = regexp.MustCompile(`^/rest/api/2/issue/([A-Z][A-Z0-9_]*-\d+)` +
		`(/comment|/transitions)?$`)
	jqlRe = regexp.MustCompile(`^project = "([A-Z0-9_]+)"( AND statusCategory != Done)?` +
		`( AND updated >= -(\d+)m)? ORDER BY updated ASC$`)
)

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Requests = append(s.Requests, r.Method+" "+r.URL.Path)
	switch {
	case s.limited > 0:
		s.limited--
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	case r.Header.Get("Authorization") != s.Authorization || s.expired:
		w.WriteHeader(http.StatusUnauthorized)
		return
	case s.disabled:
		http.Error(w, `{"errorMessages":["API access is disabled for this account"]}`,
			http.StatusForbidden)
		return
	case r.Method != http.MethodGet && r.Header.Get("X-Atlassian-Token") != "no-check":
		w.WriteHeader(http.StatusForbidden)
		return
	}
	p := r.URL.Path
	switch {
	case p == "/rest/api/2/serverInfo":
		kind := "DataCenter"
		if s.Cloud {
			kind = "Cloud"
		}
		write(w, map[string]any{"deploymentType": kind, "version": "10.0.0"})
	case p == "/rest/api/2/myself":
		write(w, s.Me)
	case p == "/rest/api/2/user/permission/search":
		s.permissionSearch(w, r)
	case p == "/rest/api/2/search/jql" && s.Cloud, p == "/rest/api/2/search" && !s.Cloud:
		s.search(w, r)
	case issuePath.MatchString(p):
		m := issuePath.FindStringSubmatch(p)
		i, ok := s.issues[m[1]]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		s.issue(w, r, i, m[2])
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *Server) issue(w http.ResponseWriter, r *http.Request, i *Issue, sub string) {
	switch {
	case sub == "" && r.Method == http.MethodGet:
		write(w, s.render(i, r.URL.Query().Get("expand") == "changelog"))
	case sub == "" && r.Method == http.MethodPut:
		var in struct {
			Update struct {
				Labels []map[string]string `json:"labels"`
			} `json:"update"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		for _, op := range in.Update.Labels {
			if l, ok := op["add"]; ok && !slices.Contains(i.Labels, l) {
				i.Labels = append(i.Labels, l)
			}
			if l, ok := op["remove"]; ok {
				i.Labels = slices.DeleteFunc(i.Labels, func(x string) bool { return x == l })
			}
		}
		w.WriteHeader(http.StatusNoContent)
	case sub == "/comment" && r.Method == http.MethodGet:
		s.listComments(w, r, i)
	case sub == "/comment" && r.Method == http.MethodPost:
		if !s.perms[s.ID(s.Me)]["ADD_COMMENTS"] {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		var in struct {
			Body       string           `json:"body"`
			Properties []map[string]any `json:"properties"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		w.WriteHeader(http.StatusCreated)
		write(w, s.addComment(i, s.Me, in.Body, in.Properties))
	case sub == "/transitions" && r.Method == http.MethodGet:
		write(w, map[string]any{"transitions": []map[string]any{
			{"id": "21", "name": "Start", "to": map[string]string{"name": "In Progress"}},
			{"id": "31", "name": "Finish", "to": map[string]string{"name": "Done"}}}})
	case sub == "/transitions" && r.Method == http.MethodPost:
		var in struct {
			Transition struct {
				ID string `json:"id"`
			} `json:"transition"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		s.Transited = append(s.Transited, i.Key+":"+in.Transition.ID)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) render(i *Issue, changelog bool) map[string]any {
	out := map[string]any{"id": i.ID, "key": i.Key, "fields": map[string]any{
		"summary": i.Summary, "description": i.Description, "labels": i.Labels,
		"reporter": i.Reporter, "created": i.Created.Format(Time),
		"updated": i.Updated.Format(Time),
		"project": map[string]string{"key": i.Project},
		"status": map[string]any{"name": i.Status,
			"statusCategory": map[string]string{"key": i.Category}}}}
	if changelog {
		out["changelog"] = map[string]any{"histories": i.Histories}
	}
	return out
}

func (s *Server) listComments(w http.ResponseWriter, r *http.Request, i *Issue) {
	start, _ := strconv.Atoi(r.URL.Query().Get("startAt"))
	n := s.page(r)
	all := i.Comments
	end := min(start+n, len(all))
	page := []Comment{}
	if start < len(all) {
		page = all[start:end]
	}
	if r.URL.Query().Get("expand") != "properties" {
		stripped := make([]Comment, len(page))
		for k, c := range page {
			c.Properties = nil
			stripped[k] = c
		}
		page = stripped
	}
	write(w, map[string]any{"startAt": start, "maxResults": n, "total": len(all), "comments": page})
}

func (s *Server) page(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("maxResults"))
	if n <= 0 || n > s.PageSize {
		n = s.PageSize
	}
	return n
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	m := jqlRe.FindStringSubmatch(q.Get("jql"))
	if m == nil {
		http.Error(w, `{"errorMessages":["unsupported JQL"]}`, http.StatusBadRequest)
		return
	}
	var since time.Time
	if m[4] != "" {
		mins, _ := strconv.Atoi(m[4])
		since = s.now().Add(-time.Duration(mins) * time.Minute)
	}
	var found []*Issue
	for _, i := range s.issues {
		if i.Project == m[1] && (m[2] == "" || i.Category != "done") && !i.Updated.Before(since) {
			found = append(found, i)
		}
	}
	slices.SortFunc(found, func(a, b *Issue) int {
		if c := a.Updated.Compare(b.Updated); c != 0 {
			return c
		}
		return strings.Compare(a.Key, b.Key)
	})
	start, _ := strconv.Atoi(q.Get("startAt"))
	if s.Cloud {
		start, _ = strconv.Atoi(q.Get("nextPageToken"))
	}
	n := s.page(r)
	end := min(start+n, len(found))
	out := []map[string]any{}
	for _, i := range found[min(start, len(found)):end] {
		out = append(out, s.render(i, q.Get("expand") == "changelog"))
	}
	res := map[string]any{"issues": out}
	if s.Cloud {
		if end < len(found) {
			res["nextPageToken"] = strconv.Itoa(end)
		}
	} else {
		res["startAt"], res["total"] = start, len(found)
	}
	write(w, res)
}

func (s *Server) permissionSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	who := q.Get("accountId")
	if !s.Cloud {
		who = q.Get("username")
	}
	if _, ok := s.issues[q.Get("issueKey")]; !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	users := []User{}
	if s.perms[who][q.Get("permissions")] {
		if s.Cloud {
			users = append(users, User{AccountID: who, AccountType: "atlassian"})
		} else {
			users = append(users, User{Name: who, Key: "key-" + who})
		}
	}
	write(w, users)
}

func write(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
