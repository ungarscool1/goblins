package console_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ungarscool1/goblins/internal/console"
	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/storage"
	"github.com/ungarscool1/goblins/internal/workflow"
)

const (
	opToken     = "operator-token-0123456789abcdef0123456789"
	viewerToken = "viewer-token-0123456789abcdef0123456789ab"
)

var t0 = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

type world struct {
	t     *testing.T
	store *storage.Store
	srv   *httptest.Server
	seq   int
}

func setup(t *testing.T, readOnly bool) *world {
	t.Helper()
	store, err := storage.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	h := console.Handler(console.Options{Store: store, ReadOnly: readOnly,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return t0 },
		Operators: []console.Operator{
			{Name: "ops", CanAct: true, Token: core.NewSecret(opToken)},
			{Name: "watcher", Token: core.NewSecret(viewerToken)}},
		Policies: func(string) (workflow.Policy, bool) { return workflow.DefaultPolicy(), true },
		Agents:   func(string) []string { return []string{"planner: fake, model big"} }})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &world{t: t, store: store, srv: srv}
}

// seed stores a task as the workflow would, with what it has said and run so far.
func (w *world) seed(task core.Task, design *core.Design, jobs ...core.JobDraft) {
	w.t.Helper()
	ctx := context.Background()
	w.seq++
	key := "seed-" + task.ID + "-" + string(rune('a'+w.seq))
	if _, _, err := w.store.AppendEvent(ctx, core.Event{Project: task.Project,
		Subject: task.Subject, DedupKey: key, Type: core.EventIssueOpened}); err != nil {
		w.t.Fatal(err)
	}
	ev, err := w.store.ClaimEvent(ctx, "seed", time.Minute, t0.Add(time.Hour))
	if err != nil || ev == nil {
		w.t.Fatalf("claim: %v", err)
	}
	if err := w.store.Commit(ctx, core.StateCommit{EventSeq: ev.Seq, EventOwner: "seed",
		Task: &task, Design: design, Jobs: jobs, Now: t0}); err != nil {
		w.t.Fatal(err)
	}
}

func issue(n string) core.Subject {
	return core.Subject{Kind: core.SubjectIssue, Provider: "t", Container: "o/r", ExternalID: n}
}

func (w *world) designTask() core.Task {
	task := core.Task{ID: "t-design", Project: "p", Kind: core.TaskIssue, Subject: issue("1"),
		State: core.StateDesignProposed, Title: "Add <b>echo</b>", DesignRevision: 1,
		DesignHash: strings.Repeat("ab", 32)}
	w.seed(task, &core.Design{Revision: 1, Hash: task.DesignHash, Body: "Plan: <script>x</script>"},
		core.JobDraft{Kind: workflow.JobComment, IdempotencyKey: "q",
			Payload: json.RawMessage(`{"body":"Which status code?"}`)})
	return task
}

func (w *world) do(req *http.Request) *http.Response {
	w.t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (w *world) form(path, cookie string, values url.Values) *http.Response {
	req, _ := http.NewRequest(http.MethodPost, w.srv.URL+path, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	return w.do(req)
}

func (w *world) get(path, cookie string) *http.Response {
	req, _ := http.NewRequest(http.MethodGet, w.srv.URL+path, nil)
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	return w.do(req)
}

// login returns the session cookie for a token.
func (w *world) login(token string) string {
	w.t.Helper()
	resp := w.form("/login", "", url.Values{"token": {token}})
	for _, c := range resp.Cookies() {
		if c.Name == "goblins_console" && c.Value != "" {
			if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode {
				w.t.Fatalf("weak session cookie: %+v", c)
			}
			return c.Name + "=" + c.Value
		}
	}
	w.t.Fatalf("no session: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	return ""
}

var csrfField = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (w *world) csrf(cookie, path string) string {
	w.t.Helper()
	m := csrfField.FindStringSubmatch(body(w.t, w.get(path, cookie)))
	if m == nil {
		w.t.Fatal("no form token on the page")
	}
	return m[1]
}

func (w *world) api(method, path, token, payload string) *http.Response {
	req, _ := http.NewRequest(method, w.srv.URL+path, strings.NewReader(payload))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return w.do(req)
}

// operatorComments returns what operators entered on a task, as the workflow will read it.
func (w *world) operatorComments(task core.Task) []core.Event {
	w.t.Helper()
	evs, err := w.store.ListEvents(context.Background(), task.Project, task.Subject, 0)
	if err != nil {
		w.t.Fatal(err)
	}
	var out []core.Event
	for _, ev := range evs {
		if ev.Source == workflow.SourceOperator {
			out = append(out, ev)
		}
	}
	return out
}

func TestEverythingButTheLoginNeedsAnOperator(t *testing.T) {
	w := setup(t, false)
	w.designTask()
	for _, path := range []string{"/", "/tasks/t-design", "/tasks/t-design/transcript"} {
		if resp := w.get(path, ""); resp.StatusCode != http.StatusSeeOther ||
			resp.Header.Get("Location") != "/login" {
			t.Errorf("%s without a login: %d", path, resp.StatusCode)
		}
	}
	if resp := w.get("/", "goblins_console=forged"); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("forged session: %d", resp.StatusCode)
	}
	for _, token := range []string{"", "wrong", opToken[:len(opToken)-1]} {
		if resp := w.api("GET", "/api/v1/tasks", token, ""); resp.StatusCode != 401 {
			t.Errorf("token %q: %d", token, resp.StatusCode)
		}
	}
	if resp := w.get("/login", ""); resp.StatusCode != 200 ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'none'") ||
		resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Errorf("login page headers: %v", resp.Header)
	}
	// Guessing is slowed down, and the right token still has to wait.
	for range 10 {
		w.form("/login", "", url.Values{"token": {"guess"}})
	}
	if resp := w.form("/login", "", url.Values{"token": {opToken}}); resp.StatusCode != 429 {
		t.Errorf("after ten failures: %d", resp.StatusCode)
	}
}

func TestOperatorAnswersApprovesAndPausesFromThePage(t *testing.T) {
	w := setup(t, false)
	task := w.designTask()
	cookie := w.login(opToken)
	page := body(t, w.get("/tasks/t-design", cookie))
	for _, want := range []string{"Which status code?", "Plan: &lt;script&gt;x&lt;/script&gt;",
		"Add &lt;b&gt;echo&lt;/b&gt;", "Approve revision 1", "planner: fake, model big"} {
		if !strings.Contains(page, want) {
			t.Errorf("%q is missing from the task page", want)
		}
	}
	if strings.Contains(page, "<script>") {
		t.Fatal("design text reached the page unescaped")
	}
	token := w.csrf(cookie, "/tasks/t-design")
	steps := []url.Values{
		{"action": {"answer"}, "text": {"  200, please  "}},
		{"action": {"approve"}, "revision": {"1"}, "hash": {task.DesignHash}},
		{"action": {"pause"}},
	}
	for _, v := range steps {
		v.Set("csrf", token)
		resp := w.form("/tasks/t-design/actions", cookie, v)
		if resp.StatusCode != http.StatusSeeOther ||
			!strings.HasSuffix(resp.Header.Get("Location"), "?sent") {
			t.Fatalf("%v: %d %s", v, resp.StatusCode, body(t, resp))
		}
	}
	got := w.operatorComments(task)
	want := []string{"200, please", "/approve-design 1 " + task.DesignHash, "/goblins pause"}
	if len(got) != len(want) {
		t.Fatalf("operator comments = %+v", got)
	}
	for i, ev := range got {
		if ev.Comment.Body != want[i] || ev.Actor.Login != "ops" || ev.Actor.ID != "operator:ops" {
			t.Errorf("comment %d = %q by %+v", i, ev.Comment.Body, ev.Actor)
		}
	}
	if !strings.Contains(body(t, w.get("/tasks/t-design?sent", cookie)), "Sent.") {
		t.Error("no notice after an action")
	}
	if strings.Contains(body(t, w.get("/tasks/t-design?notice=evil", cookie)), "evil") {
		t.Error("a link put its own words on the page")
	}
}

func TestFormsRefuseOtherSites(t *testing.T) {
	w := setup(t, false)
	task := w.designTask()
	cookie := w.login(opToken)
	token := w.csrf(cookie, "/")
	resp := w.form("/tasks/t-design/actions", cookie, url.Values{"action": {"pause"}})
	if resp.StatusCode != 403 {
		t.Errorf("no form token: %d", resp.StatusCode)
	}
	other := setup(t, false)
	otherToken := other.csrf(other.login(opToken), "/")
	if resp := w.form("/tasks/t-design/actions", cookie,
		url.Values{"action": {"pause"}, "csrf": {otherToken}}); resp.StatusCode != 403 {
		t.Errorf("another session's token: %d", resp.StatusCode)
	}
	for header, value := range map[string]string{"Origin": "https://evil.example",
		"Sec-Fetch-Site": "cross-site"} {
		req, _ := http.NewRequest(http.MethodPost, w.srv.URL+"/tasks/t-design/actions",
			strings.NewReader(url.Values{"action": {"pause"}, "csrf": {token}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Cookie", cookie)
		req.Header.Set(header, value)
		if resp := w.do(req); resp.StatusCode != 403 {
			t.Errorf("%s %s: %d", header, value, resp.StatusCode)
		}
	}
	if got := w.operatorComments(task); len(got) != 0 {
		t.Fatalf("a refused request still acted: %+v", got)
	}
	// Logging out ends the session for good.
	w.form("/logout", cookie, url.Values{"csrf": {token}})
	if resp := w.get("/", cookie); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("after logout: %d", resp.StatusCode)
	}
}

func TestViewersAndReadOnlyConsolesChangeNothing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		readOnly bool
		token    string
	}{{"viewer", false, viewerToken}, {"read-only console", true, opToken}} {
		t.Run(tc.name, func(t *testing.T) {
			w := setup(t, tc.readOnly)
			task := w.designTask()
			cookie := w.login(tc.token)
			page := body(t, w.get("/tasks/t-design", cookie))
			if strings.Contains(page, "/actions") || !strings.Contains(page, "read-only") {
				t.Error("the page offers actions")
			}
			token := w.csrf(cookie, "/")
			if resp := w.form("/tasks/t-design/actions", cookie,
				url.Values{"action": {"cancel"}, "csrf": {token}}); resp.StatusCode != 403 {
				t.Errorf("form: %d", resp.StatusCode)
			}
			if resp := w.api("POST", "/api/v1/tasks/t-design/actions", tc.token,
				`{"action":"cancel"}`); resp.StatusCode != 403 {
				t.Errorf("api: %d", resp.StatusCode)
			}
			if got := w.operatorComments(task); len(got) != 0 {
				t.Fatalf("acted: %+v", got)
			}
		})
	}
}

func TestAPIGivesTheQueueTheTaskAndItsConversation(t *testing.T) {
	w := setup(t, false)
	task := w.designTask()
	w.seed(core.Task{ID: "t-run", Project: "p", Kind: core.TaskIssue, Subject: issue("2"),
		State: core.StateImplementing, CurrentJob: &core.JobDraft{Kind: workflow.JobImplement}},
		nil)
	w.seed(core.Task{ID: "t-done", Project: "p", Kind: core.TaskIssue, Subject: issue("3"),
		State: core.StateCompleted}, nil)
	ctx := context.Background()
	comment := func(id, author, text string, typ core.EventType, actor core.ActorType) {
		_, _, err := w.store.AppendEvent(ctx, core.Event{Project: "p", Subject: task.Subject,
			DedupKey: id + text, Type: typ, OccurredAt: t0.Add(time.Minute),
			Comment: &core.Comment{ID: id, Body: text, CreatedAt: t0.Add(time.Minute),
				Author: core.Actor{Login: author, Type: actor}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	comment("c1", "alice", "first wording", core.EventCommentCreated, core.ActorHuman)
	comment("c1", "alice", "second wording", core.EventCommentEdited, core.ActorHuman)
	comment("c2", "goblins[bot]", "echo of our own comment", core.EventCommentCreated,
		core.ActorBot)

	var q console.Queue
	decode(t, w.api("GET", "/api/v1/tasks", viewerToken, ""), &q)
	if len(q.NeedsYou) != 1 || q.NeedsYou[0].ID != "t-design" || len(q.Working) != 1 ||
		q.Working[0].Working != "implement" || len(q.Finished) != 1 {
		t.Fatalf("queue = %+v", q)
	}
	var conv []console.Message
	decode(t, w.api("GET", "/api/v1/tasks/t-design/conversation", viewerToken, ""), &conv)
	if len(conv) != 2 || conv[0].Text != "Which status code?" || conv[0].From != "goblins" ||
		conv[0].Delivery != "not posted yet" || conv[1].Text != "second wording" ||
		!conv[1].Edited {
		t.Fatalf("conversation = %+v", conv)
	}
	var d console.Detail
	decode(t, w.api("GET", "/api/v1/tasks/t-design", viewerToken, ""), &d)
	if d.Design == nil || d.Design.Revision != 1 || len(d.Actions) == 0 ||
		d.Actions[0] != "approve" {
		t.Fatalf("detail = %+v", d)
	}
	for payload, code := range map[string]int{`{"action":"answer","text":"/goblins cancel"}`: 400,
		`{"action":"approve","revision":1}`: 400, `{"action":"dance"}`: 400, `not json`: 400,
		`{"action":"answer","text":"plain text, please"}`: 202} {
		resp := w.api("POST", "/api/v1/tasks/t-design/actions", opToken, payload)
		if resp.StatusCode != code {
			t.Errorf("%s: %d, want %d", payload, resp.StatusCode, code)
		}
	}
	resp := w.api("POST", "/api/v1/tasks/t-done/actions", opToken, `{"action":"cancel"}`)
	if resp.StatusCode != 409 {
		t.Errorf("finished task: %d", resp.StatusCode)
	}
	if resp := w.api("GET", "/api/v1/tasks/nope", opToken, ""); resp.StatusCode != 404 {
		t.Errorf("unknown task: %d", resp.StatusCode)
	}
}

func TestTranscriptsAreShownEscapedAndPerJob(t *testing.T) {
	w := setup(t, false)
	w.designTask()
	jobs, _ := w.store.ListJobs(context.Background(), "t-design")
	err := w.store.AppendTranscript(context.Background(), []core.TranscriptEntry{
		{TaskID: "t-design", JobID: jobs[0].ID, Kind: "text", At: t0,
			Text: "<img src=x onerror=alert(1)>"},
		{TaskID: "t-design", JobID: "other", Kind: "text", Text: "from another job", At: t0}})
	if err != nil {
		t.Fatal(err)
	}
	cookie := w.login(opToken)
	page := body(t, w.get("/tasks/t-design/transcript?job="+jobs[0].ID, cookie))
	if !strings.Contains(page, "&lt;img src=x") || strings.Contains(page, "<img") ||
		strings.Contains(page, "from another job") {
		t.Fatalf("transcript page:\n%s", page)
	}
	var tr console.Transcript
	decode(t, w.api("GET", "/api/v1/tasks/t-design/transcript", viewerToken, ""), &tr)
	if len(tr.Entries) != 2 {
		t.Fatalf("transcript = %+v", tr)
	}
}

// Every part of the task page renders, for a task that has been through everything.
func TestAFullTaskPageRenders(t *testing.T) {
	w := setup(t, false)
	w.seed(core.Task{ID: "t-full", Project: "p", Kind: core.TaskIssue, Subject: issue("4"),
		State: core.StateBlocked, Reason: "paused by ops", Title: "Full", DesignRevision: 3,
		DesignHash: "abcdef0123", PausedUntil: t0.Add(time.Hour), HeadSHA: "deadbeef",
		ValidatedSHA: "deadbeef", Loops: map[string]int{"review_fix": 2},
		Approval: &core.Approval{Revision: 3, Approver: core.Actor{Login: "alice"}, At: t0},
		PullRequest: &core.PullRequest{Number: "7", URL: "https://example.test/pr/7",
			Draft: true},
		ValidatedReport: &core.ValidationReport{Status: core.ValidationPassed, Summary: "unit ok"},
		CurrentJob:      &core.JobDraft{Kind: workflow.JobImplement}},
		&core.Design{Revision: 3, Hash: "abcdef0123", Body: "third"})
	cookie := w.login(opToken)
	resp := w.get("/tasks/t-full", cookie)
	page := body(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("%d: %s", resp.StatusCode, page)
	}
	for _, want := range []string{"paused by ops", "/tasks/t-full/designs/1",
		"/tasks/t-full/designs/2",
		"https://example.test/pr/7", "(draft)", "unit ok", "review_fix: 2", "revision 3 by alice",
		"Resume, running the interrupted step again", "implement"} {
		if !strings.Contains(page, want) {
			t.Errorf("%q is missing", want)
		}
	}
	if strings.Contains(page, "Approve revision") {
		t.Error("a blocked task offers an approval")
	}
	if resp := w.get("/", cookie); resp.StatusCode != 200 ||
		!strings.Contains(body(t, resp), "Full") {
		t.Error("the queue page does not list the task")
	}
}

func decode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	if resp.StatusCode != 200 {
		t.Fatalf("%s: %d", resp.Request.URL, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}
