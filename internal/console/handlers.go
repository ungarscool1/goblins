package console

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"github.com/ungarscool1/goblins/internal/core"
)

// View is what every page knows about who is looking.
type View struct {
	Operator string
	CanAct   bool
	CSRF     string
	Notice   string
}

func (c *console) viewFor(r *http.Request) View {
	ses := sessionOf(r)
	v := View{Operator: ses.op.Name, CanAct: ses.op.CanAct && !c.o.ReadOnly, CSRF: ses.csrf}
	// A fixed text, so a link cannot put words of its own on the page.
	if r.URL.Query().Has("sent") {
		v.Notice = "Sent. The workflow applies it in a moment, or refuses it as it would " +
			"the same comment on the ticket. Reload to see what it did."
	}
	return v
}

func (c *console) index(w http.ResponseWriter, r *http.Request) {
	q, err := c.queue(r.Context())
	if err != nil {
		c.fail(w, err)
		return
	}
	c.render(w, "index.html", struct {
		View
		Queue
	}{c.viewFor(r), q})
}

func (c *console) taskPage(w http.ResponseWriter, r *http.Request) {
	d, err := c.detail(r.Context(), r.PathValue("id"))
	if err != nil {
		c.fail(w, err)
		return
	}
	c.render(w, "task.html", struct {
		View
		Detail
		Can map[string]bool
	}{c.viewFor(r), d, can(d.Actions)})
}

func can(actions []string) map[string]bool {
	out := map[string]bool{}
	for _, a := range actions {
		out[a] = true
	}
	return out
}

func (c *console) transcriptPage(w http.ResponseWriter, r *http.Request) {
	tr, err := c.transcript(r.Context(), r.PathValue("id"), r.URL.Query().Get("job"))
	if err != nil {
		c.fail(w, err)
		return
	}
	c.render(w, "transcript.html", struct {
		View
		Transcript
		TaskID string
	}{c.viewFor(r), tr, r.PathValue("id")})
}

func (c *console) designPage(w http.ResponseWriter, r *http.Request) {
	d, err := c.design(r.Context(), r)
	if err != nil {
		c.fail(w, err)
		return
	}
	c.render(w, "design.html", struct {
		View
		Design core.Design
	}{c.viewFor(r), d})
}

func (c *console) actionForm(w http.ResponseWriter, r *http.Request) {
	rev, _ := strconv.Atoi(r.PostFormValue("revision"))
	a := Action{Action: r.PostFormValue("action"), Text: r.PostFormValue("text"),
		Revision: rev, Hash: r.PostFormValue("hash")}
	id := r.PathValue("id")
	if err := c.act(r.Context(), operatorOf(r), id, a); err != nil {
		c.refuse(w, err)
		return
	}
	http.Redirect(w, r, "/tasks/"+url.PathEscape(id)+"?sent", http.StatusSeeOther)
}

// refuse says why an action was not taken. Unlike other errors, the reason is the operator's
// own doing, so it is safe and useful to show.
func (c *console) refuse(w http.ResponseWriter, err error) {
	var code int
	switch {
	case core.IsKind(err, core.KindForbidden):
		code = http.StatusForbidden
	case core.IsKind(err, core.KindConflict):
		code = http.StatusConflict
	case core.IsKind(err, core.KindInvalid):
		code = http.StatusBadRequest
	case core.IsKind(err, core.KindNotFound):
		code = http.StatusNotFound
	default:
		c.fail(w, err)
		return
	}
	http.Error(w, err.Error(), code)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (c *console) apiTasks(w http.ResponseWriter, r *http.Request) {
	q, err := c.queue(r.Context())
	if err != nil {
		c.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, q)
}

func (c *console) apiTask(w http.ResponseWriter, r *http.Request) {
	d, err := c.detail(r.Context(), r.PathValue("id"))
	if err != nil {
		c.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (c *console) apiConversation(w http.ResponseWriter, r *http.Request) {
	d, err := c.detail(r.Context(), r.PathValue("id"))
	if err != nil {
		c.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d.Conversation)
}

func (c *console) apiTranscript(w http.ResponseWriter, r *http.Request) {
	tr, err := c.transcript(r.Context(), r.PathValue("id"), r.URL.Query().Get("job"))
	if err != nil {
		c.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tr)
}

func (c *console) apiDesign(w http.ResponseWriter, r *http.Request) {
	d, err := c.design(r.Context(), r)
	if err != nil {
		c.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (c *console) apiAction(w http.ResponseWriter, r *http.Request) {
	var a Action
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		http.Error(w, "the body must be a JSON action", http.StatusBadRequest)
		return
	}
	if err := c.act(r.Context(), operatorOf(r), r.PathValue("id"), a); err != nil {
		c.refuse(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}
