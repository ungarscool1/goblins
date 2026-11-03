// Package console is the operator console: a few server-rendered pages and a JSON API to
// watch tasks, read their conversation and transcripts, answer questions and act on tasks.
//
// It never changes a task itself. Every action is entered as an operator comment, the same
// event `goblins comment` enters, so the workflow applies its usual rules and the event log
// records who did what. See docs/adr/0006-operator-console.md.
package console

import (
	"bytes"
	"context"
	"embed"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/workflow"
)

// Store is what the console reads, plus the one write it makes: appending an event.
type Store interface {
	GetTask(ctx context.Context, id string) (core.Task, error)
	ListTasks(ctx context.Context, project string) ([]core.Task, error)
	ListTransitions(ctx context.Context, taskID string) ([]core.Transition, error)
	ListJobs(ctx context.Context, taskID string) ([]core.Job, error)
	ListTranscript(ctx context.Context, taskID string, limit int) ([]core.TranscriptEntry, error)
	GetDesign(ctx context.Context, taskID string, revision int) (core.Design, error)
	ListEvents(ctx context.Context, project string, subject core.Subject,
		limit int) ([]core.Event, error)
	AppendEvent(ctx context.Context, ev core.Event) (core.Event, bool, error)
}

// Options configures the console.
type Options struct {
	Store     Store
	Operators []Operator
	// Policies gives each project's approval command.
	Policies workflow.Policies
	// Agents describes who works on a project, one line per stage, such as
	// "planner: main, model large".
	Agents         func(project string) []string
	ReadOnly       bool
	SessionTTL     time.Duration
	InsecureCookie bool
	Log            *slog.Logger
	Now            func() time.Time
}

//go:embed ui
var ui embed.FS

var pages = template.Must(template.New("").Funcs(template.FuncMap{
	"when": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format("2006-01-02 15:04:05Z")
	},
	// earlier lists the revisions before one.
	"earlier": func(rev int) []int {
		out := []int{}
		for i := 1; i < rev; i++ {
			out = append(out, i)
		}
		return out
	},
	"button": func(task, csrf, action, label string) map[string]string {
		return map[string]string{"Task": task, "CSRF": csrf, "Action": action, "Label": label}
	},
}).ParseFS(ui, "ui/*.html"))

type console struct {
	o        Options
	sessions *sessions
	throttle *throttle
}

// Handler returns the console's routes. Every route but the login page and the stylesheet
// needs an operator: pages through a browser session, the API through a bearer token.
func Handler(o Options) http.Handler {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.SessionTTL <= 0 {
		o.SessionTTL = 12 * time.Hour
	}
	if o.Agents == nil {
		o.Agents = func(string) []string { return nil }
	}
	c := &console{o: o, sessions: newSessions(o.SessionTTL, o.Now), throttle: &throttle{now: o.Now}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /static/style.css", c.style)
	mux.HandleFunc("GET /login", c.loginPage)
	mux.HandleFunc("POST /login", c.login)
	mux.HandleFunc("POST /logout", c.logout)
	mux.Handle("GET /{$}", c.page(c.index))
	mux.Handle("GET /tasks/{id}", c.page(c.taskPage))
	mux.Handle("GET /tasks/{id}/transcript", c.page(c.transcriptPage))
	mux.Handle("GET /tasks/{id}/designs/{rev}", c.page(c.designPage))
	mux.Handle("POST /tasks/{id}/actions", c.form(c.actionForm))
	mux.Handle("GET /api/v1/tasks", c.api(c.apiTasks))
	mux.Handle("GET /api/v1/tasks/{id}", c.api(c.apiTask))
	mux.Handle("GET /api/v1/tasks/{id}/conversation", c.api(c.apiConversation))
	mux.Handle("GET /api/v1/tasks/{id}/transcript", c.api(c.apiTranscript))
	mux.Handle("GET /api/v1/tasks/{id}/designs/{rev}", c.api(c.apiDesign))
	mux.Handle("POST /api/v1/tasks/{id}/actions", c.api(c.apiAction))
	return headers(mux)
}

// headers keeps pages out of frames and caches, and allows no script at all.
func headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; "+
			"form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (c *console) style(w http.ResponseWriter, _ *http.Request) {
	data, err := ui.ReadFile("ui/style.css")
	if err != nil {
		http.Error(w, "missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	_, _ = w.Write(data)
}

// render writes a page whole or not at all, so a failure is never half a page.
func (c *console) render(w http.ResponseWriter, name string, data any) {
	var b bytes.Buffer
	if err := pages.ExecuteTemplate(&b, name, data); err != nil {
		c.fail(w, core.Wrap(core.KindInternal, "console.render", err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = b.WriteTo(w)
}

// fail answers an error without saying more than its kind: details stay in the logs.
func (c *console) fail(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case core.IsKind(err, core.KindNotFound):
		code = http.StatusNotFound
	case core.IsKind(err, core.KindInvalid):
		code = http.StatusBadRequest
	default:
		c.o.Log.Error("console request failed", "err", err)
	}
	http.Error(w, http.StatusText(code), code)
}
