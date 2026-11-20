package console_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ungarscool1/goblins/internal/console"
	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/workflow"
)

const scopedToken = "scoped-token-0123456789abcdef0123456789ab"

// An operator limited to a project sees, and acts on, that project only: another project's
// task does not exist for them.
func TestOperatorsSeeTheirProjectsOnly(t *testing.T) {
	w := setupWith(t, false, console.Operator{Name: "billing-lead", CanAct: true,
		Token: core.NewSecret(scopedToken), Projects: []string{"billing"}})
	w.seed(core.Task{ID: "t-bill", Project: "billing", Kind: core.TaskIssue, Subject: issue("7"),
		State: core.StateBlocked, Title: "Invoices"}, nil)
	other := w.designTask() // project p
	var q struct {
		NeedsYou []struct {
			ID string `json:"id"`
		} `json:"needs_you"`
	}
	resp := w.api(http.MethodGet, "/api/v1/tasks", scopedToken, "")
	if err := json.NewDecoder(resp.Body).Decode(&q); err != nil {
		t.Fatal(err)
	}
	if len(q.NeedsYou) != 1 || q.NeedsYou[0].ID != "t-bill" {
		t.Fatalf("queue = %+v", q)
	}
	for _, path := range []string{"/api/v1/tasks/" + other.ID, "/api/v1/tasks/" + other.ID +
		"/transcript", "/api/v1/tasks/" + other.ID + "/designs/1"} {
		if resp := w.api(http.MethodGet, path, scopedToken,
			""); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d", path, resp.StatusCode)
		}
	}
	act := w.api(http.MethodPost, "/api/v1/tasks/"+other.ID+"/actions", scopedToken,
		`{"action":"pause"}`)
	if act.StatusCode != http.StatusNotFound || len(w.operatorComments(other)) != 0 {
		t.Fatalf("acted on another project: %d", act.StatusCode)
	}
	if resp := w.api(http.MethodGet, "/api/v1/tasks/t-bill", scopedToken,
		""); resp.StatusCode != http.StatusOK {
		t.Fatalf("own task: %d", resp.StatusCode)
	}
	// Unlimited operators still see everything.
	if resp := w.api(http.MethodGet, "/api/v1/tasks/"+other.ID, opToken,
		""); resp.StatusCode != http.StatusOK {
		t.Fatalf("unlimited operator: %d", resp.StatusCode)
	}
}

// A failed task can be run again from the console, and nothing else can be done to it.
func TestAFailedTaskCanBeRunAgain(t *testing.T) {
	w := setup(t, false)
	task := core.Task{ID: "t-failed", Project: "p", Kind: core.TaskIssue, Subject: issue("9"),
		State: core.StateFailed, Title: "Broken", ResumeState: core.StateImplementing}
	w.seed(task, nil)
	if resp := w.api(http.MethodPost, "/api/v1/tasks/t-failed/actions", opToken,
		`{"action":"pause"}`); resp.StatusCode != http.StatusConflict {
		t.Fatalf("pause on a failed task: %d", resp.StatusCode)
	}
	if resp := w.api(http.MethodPost, "/api/v1/tasks/t-failed/actions", opToken,
		`{"action":"retry"}`); resp.StatusCode/100 != 2 {
		t.Fatalf("retry: %d %s", resp.StatusCode, body(t, resp))
	}
	evs := w.operatorComments(task)
	if len(evs) != 1 || evs[0].Comment.Body != "/goblins retry" {
		t.Fatalf("comments = %+v", evs)
	}
	page := body(t, w.get("/tasks/t-failed", w.login(opToken)))
	if !strings.Contains(page, `value="retry"`) {
		t.Fatal("the page offers no retry")
	}
}

// Each transcript entry says which run it came from.
func TestTranscriptEntriesNameTheirRun(t *testing.T) {
	w := setup(t, false)
	task := core.Task{ID: "t-run", Project: "p", Kind: core.TaskIssue, Subject: issue("3"),
		State: core.StateImplementing, Title: "Run", HeadSHA: "head123"}
	w.seed(task, nil, core.JobDraft{Kind: workflow.JobValidate, IdempotencyKey: "v",
		Payload: json.RawMessage(`{"sha":"abc123"}`)})
	jobs, _ := w.store.ListJobs(t.Context(), "t-run")
	if err := w.store.AppendTranscript(t.Context(), []core.TranscriptEntry{{TaskID: "t-run",
		JobID: jobs[0].ID, Kind: "validation", Text: "ok", At: t0}}); err != nil {
		t.Fatal(err)
	}
	var tr struct {
		Entries []struct {
			Stage  string `json:"stage"`
			Commit string `json:"commit"`
		} `json:"entries"`
	}
	resp := w.api(http.MethodGet, "/api/v1/tasks/t-run/transcript", opToken, "")
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		t.Fatal(err)
	}
	if len(tr.Entries) != 1 || tr.Entries[0].Stage != "validate" ||
		tr.Entries[0].Commit != "abc123" {
		t.Fatalf("entries = %+v", tr.Entries)
	}
	page := body(t, w.get("/tasks/t-run/transcript", w.login(opToken)))
	if !strings.Contains(page, "validate, attempt") || !strings.Contains(page,
		"<code>abc123</code>") {
		t.Fatalf("page:\n%s", page)
	}
}
