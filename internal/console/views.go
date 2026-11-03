package console

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/workflow"
)

// Summary is one line of the queue.
type Summary struct {
	ID          string     `json:"id"`
	Project     string     `json:"project"`
	Title       string     `json:"title"`
	State       core.State `json:"state"`
	Reason      string     `json:"reason,omitempty"`
	Working     string     `json:"working_on,omitempty"`
	PullRequest string     `json:"pull_request,omitempty"`
	PausedUntil time.Time  `json:"paused_until,omitzero"`
	Updated     time.Time  `json:"updated_at"`
}

// Queue sorts tasks by who has to move next.
type Queue struct {
	NeedsYou []Summary `json:"needs_you"`
	Working  []Summary `json:"working"`
	Finished []Summary `json:"finished"`
}

// needsPerson lists the states where nothing happens until a person acts.
var needsPerson = map[core.State]bool{core.StateWaitingForUser: true,
	core.StateDesignProposed: true, core.StateBlocked: true, core.StateAwaitingReview: true}

// maxFinished bounds the finished tasks the queue lists.
const maxFinished = 50

func summarize(t core.Task) Summary {
	s := Summary{ID: t.ID, Project: t.Project, Title: t.Title, State: t.State, Reason: t.Reason,
		PausedUntil: t.PausedUntil, Updated: t.UpdatedAt}
	if t.CurrentJob != nil {
		s.Working = string(t.CurrentJob.Kind)
	}
	if pr := t.PullRequest; pr != nil {
		s.PullRequest = cmp.Or(pr.URL, "#"+pr.Number)
	}
	return s
}

func (c *console) queue(ctx context.Context) (Queue, error) {
	tasks, err := c.o.Store.ListTasks(ctx, "")
	if err != nil {
		return Queue{}, err
	}
	slices.SortStableFunc(tasks, func(a, b core.Task) int {
		return b.UpdatedAt.Compare(a.UpdatedAt)
	})
	q := Queue{NeedsYou: []Summary{}, Working: []Summary{}, Finished: []Summary{}}
	for _, t := range tasks {
		switch s := summarize(t); {
		case needsPerson[t.State]:
			q.NeedsYou = append(q.NeedsYou, s)
		case t.State.IsTerminal():
			if len(q.Finished) < maxFinished {
				q.Finished = append(q.Finished, s)
			}
		default:
			q.Working = append(q.Working, s)
		}
	}
	return q, nil
}

// Message is one entry of a task's conversation: what people said on the ticket or in the
// console, and what Goblins said back.
type Message struct {
	At     time.Time `json:"at"`
	Author string    `json:"author"`
	// From is "person", "operator" or "goblins".
	From   string `json:"from"`
	Text   string `json:"text"`
	Edited bool   `json:"edited,omitempty"`
	// Delivery says when something Goblins said has not reached the ticket, or never will.
	Delivery string `json:"delivery,omitempty"`
}

// Detail is everything the console shows about one task.
type Detail struct {
	Task         core.Task         `json:"task"`
	Agents       []string          `json:"agents"`
	Design       *core.Design      `json:"design,omitempty"`
	Conversation []Message         `json:"conversation"`
	Transitions  []core.Transition `json:"transitions"`
	Jobs         []core.Job        `json:"jobs"`
	Actions      []string          `json:"actions"`
}

func (c *console) detail(ctx context.Context, id string) (Detail, error) {
	t, err := c.o.Store.GetTask(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	d := Detail{Task: t, Agents: c.o.Agents(t.Project), Actions: actionsIn(t.State)}
	if d.Transitions, err = c.o.Store.ListTransitions(ctx, id); err != nil {
		return Detail{}, err
	}
	if d.Jobs, err = c.o.Store.ListJobs(ctx, id); err != nil {
		return Detail{}, err
	}
	if t.DesignRevision > 0 {
		des, err := c.o.Store.GetDesign(ctx, id, t.DesignRevision)
		if err != nil {
			return Detail{}, err
		}
		d.Design = &des
	}
	events, err := c.o.Store.ListEvents(ctx, t.Project, t.Subject, 500)
	if err != nil {
		return Detail{}, err
	}
	d.Conversation = conversation(events, d.Jobs)
	return d, nil
}

// conversation rebuilds what was said from the durable record alone, so it survives restarts
// and replaced containers. People's comments come from the events they arrived as, with the
// latest edit winning; Goblins' side comes from the jobs that posted it.
func conversation(events []core.Event, jobs []core.Job) []Message {
	out := []Message{}
	at := map[string]int{}
	for _, ev := range events {
		cm := ev.Comment
		if cm == nil || cm.Author.Type == core.ActorBot ||
			(ev.Type != core.EventCommentCreated && ev.Type != core.EventCommentEdited) {
			continue
		}
		m := Message{At: cmp.Or(cm.CreatedAt, ev.OccurredAt), Author: cm.Author.Login,
			From: "person", Text: cm.Body}
		if ev.Source == workflow.SourceOperator {
			m.From = "operator"
		}
		if i, ok := at[cm.ID]; ok {
			out[i].Text, out[i].Edited = cm.Body, true
			continue
		}
		at[cm.ID] = len(out)
		out = append(out, m)
	}
	for _, j := range jobs {
		if j.Kind != workflow.JobComment && j.Kind != workflow.JobReply {
			continue
		}
		var p struct {
			Body string `json:"body"`
		}
		if json.Unmarshal(j.Payload, &p) != nil || p.Body == "" {
			continue
		}
		out = append(out, Message{At: j.CreatedAt, Author: "Goblins", From: "goblins",
			Text: p.Body, Delivery: delivery(j)})
	}
	slices.SortStableFunc(out, func(a, b Message) int { return a.At.Compare(b.At) })
	return out
}

func delivery(j core.Job) string {
	switch j.State {
	case core.JobPending, core.JobRunning:
		return "not posted yet"
	case core.JobFailed:
		return "not posted: " + j.LastError
	case core.JobCancelled:
		return "not posted: cancelled"
	}
	return ""
}

// actionsIn lists what an operator may ask for in a state. The workflow still decides: these
// only keep the page from offering what it would refuse.
func actionsIn(s core.State) []string {
	switch {
	case s.IsTerminal():
		return []string{}
	case s == core.StateDesignProposed:
		return []string{actApprove, actAnswer, actPause, actCancel}
	case s == core.StateWaitingForUser || s == core.StateInvestigating:
		return []string{actAnswer, actPause, actCancel}
	case s == core.StateBlocked || s == core.StateQuotaPaused:
		return []string{actResume, actCancel}
	}
	return []string{actPause, actCancel}
}

// Transcript is a task's agent output, optionally for one job.
type Transcript struct {
	Job     string                 `json:"job,omitempty"`
	Jobs    []core.Job             `json:"jobs"`
	Entries []core.TranscriptEntry `json:"entries"`
}

// maxTranscript bounds the entries a page shows.
const maxTranscript = 2000

func (c *console) transcript(ctx context.Context, id, job string) (Transcript, error) {
	if _, err := c.o.Store.GetTask(ctx, id); err != nil {
		return Transcript{}, err
	}
	jobs, err := c.o.Store.ListJobs(ctx, id)
	if err != nil {
		return Transcript{}, err
	}
	all, err := c.o.Store.ListTranscript(ctx, id, maxTranscript)
	if err != nil {
		return Transcript{}, err
	}
	tr := Transcript{Job: job, Jobs: jobs, Entries: all}
	if job != "" {
		tr.Entries = slices.DeleteFunc(slices.Clone(all), func(e core.TranscriptEntry) bool {
			return e.JobID != job
		})
	}
	return tr, nil
}

func (c *console) design(ctx context.Context, r *http.Request) (core.Design, error) {
	rev, err := strconv.Atoi(r.PathValue("rev"))
	if err != nil || rev < 1 {
		return core.Design{}, core.Errorf(core.KindInvalid, "console.design", "bad revision")
	}
	return c.o.Store.GetDesign(ctx, r.PathValue("id"), rev)
}
