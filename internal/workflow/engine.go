package workflow

import (
	"context"
	"log/slog"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

// Authorizer looks up an actor's access on the tracker of a project.
type Authorizer interface {
	Permission(ctx context.Context, project string, subject core.Subject, actor core.Actor) (core.Permission, error)
}

// Policies returns the workflow rules of a project.
type Policies func(project string) (Policy, bool)

// Engine applies events to tasks. It is the only writer of task state.
type Engine struct {
	Store    core.StateStore
	Auth     Authorizer
	Policies Policies
	Log      *slog.Logger
	Owner    string
	Now      func() time.Time
	// EventLease bounds how long a crashed worker can hold up a subject.
	EventLease time.Duration
	// RetryDelay spaces attempts when a lookup fails.
	RetryDelay time.Duration
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now().UTC()
}

func (e *Engine) lease() time.Duration {
	if e.EventLease > 0 {
		return e.EventLease
	}
	return time.Minute
}

func needsPermission(ev core.Event) bool {
	switch ev.Type {
	case core.EventIssueOpened, core.EventIssueLabeled, core.EventIssueEdited,
		core.EventCommentCreated, core.EventCommentEdited, core.EventPROpened, core.EventPRReopened:
		return ev.Actor.Type == core.ActorHuman
	}
	return false
}

// ProcessNext handles one event; it reports whether there was one.
func (e *Engine) ProcessNext(ctx context.Context) (bool, error) {
	now := e.now()
	ev, err := e.Store.ClaimEvent(ctx, e.Owner, e.lease(), now)
	if err != nil || ev == nil {
		return false, err
	}
	if err := e.process(ctx, *ev, now); err != nil {
		retry := e.RetryDelay
		if retry == 0 {
			retry = 5 * time.Second
		}
		if rerr := e.Store.ReleaseEvent(ctx, ev.Seq, e.Owner, now.Add(retry)); rerr != nil {
			e.Log.Warn("cannot release event", "seq", ev.Seq, "err", rerr)
		}
		return true, err
	}
	return true, nil
}

// Drain processes events until none is claimable.
func (e *Engine) Drain(ctx context.Context) error {
	for {
		more, err := e.ProcessNext(ctx)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
}

func (e *Engine) process(ctx context.Context, ev core.Event, now time.Time) error {
	policy, ok := e.Policies(ev.Project)
	if !ok {
		e.Log.Warn("event for an unknown project dropped", "project", ev.Project, "seq", ev.Seq)
		return e.Store.Commit(ctx, core.StateCommit{EventSeq: ev.Seq, EventOwner: e.Owner, Now: now})
	}
	var current *core.Task
	switch t, err := e.Store.FindTask(ctx, ev.Project, ev.Subject); {
	case err == nil:
		current = &t
	case !core.IsKind(err, core.KindNotFound):
		return err
	}

	facts := Facts{Now: now}
	if needsPermission(ev) && (current == nil || !Terminal(current.State)) {
		perm, err := e.Auth.Permission(ctx, ev.Project, ev.Subject, ev.Actor)
		switch {
		case err == nil:
			facts.ActorPermission = perm
		case core.IsKind(err, core.KindTransient), core.IsKind(err, core.KindCancelled):
			// Unknown is not the same as denied: try again rather than reject a
			// legitimate approval because of a network blip.
			return err
		default:
			// Authorization fails closed.
			e.Log.Warn("permission lookup failed, treating as none", "actor", ev.Actor.Login, "err", err)
		}
	}

	d := Decide(current, ev, facts, policy)
	commit := core.StateCommit{EventSeq: ev.Seq, EventOwner: e.Owner, Now: now}
	if d.Task == nil {
		e.Log.Debug("event had no effect", "seq", ev.Seq, "type", ev.Type, "why", d.Ignored)
		return e.Store.Commit(ctx, commit)
	}
	resumeTo := d.Task.ResumeState
	if current != nil && current.ResumeState != "" {
		resumeTo = current.ResumeState
	}
	if !CheckTransition(d.From, d.Task.State, resumeTo) {
		// A bug in the table or machine must stop the task, not corrupt it.
		e.Log.Error("illegal transition refused", "task", d.Task.ID, "from", d.From, "to", d.Task.State)
		return core.Errorf(core.KindInternal, "workflow.process", "illegal transition %s -> %s", d.From, d.Task.State)
	}
	commit.Task, commit.Design, commit.Jobs, commit.CancelKinds = d.Task, d.Design, d.Jobs, d.CancelKinds
	if current != nil {
		commit.ExpectedVersion = current.Version
	}
	if d.From != d.Task.State || d.IsNew {
		commit.Transition = &core.Transition{TaskID: d.Task.ID, From: d.From, To: d.Task.State, Reason: d.Reason, EventSeq: ev.Seq, At: now}
	}
	if err := e.Store.Commit(ctx, commit); err != nil {
		return err
	}
	if commit.Transition != nil {
		e.Log.Info("task transition", "task", d.Task.ID, "from", d.From, "to", d.Task.State, "reason", d.Reason)
	}
	return nil
}

// JobOutcomeEvent builds the event reporting a job's end. Its dedup key makes a
// worker's retry after a lost response harmless.
func JobOutcomeEvent(task core.Task, job core.Job, o core.JobOutcome, now time.Time) core.Event {
	o.JobID, o.Kind, o.Key = job.ID, job.Kind, job.IdempotencyKey
	return core.Event{
		Project: job.Project, Source: "engine", Type: core.EventJobFinished, Subject: task.Subject,
		DedupKey: "job:" + job.ID + ":finished", OccurredAt: now, ReceivedAt: now, Job: &o,
	}
}

// JobStartedEvent builds the event reporting that a job began.
func JobStartedEvent(task core.Task, job core.Job, now time.Time) core.Event {
	return core.Event{
		Project: job.Project, Source: "engine", Type: core.EventJobStarted, Subject: task.Subject,
		DedupKey: "job:" + job.ID + ":started", OccurredAt: now, ReceivedAt: now,
		Job: &core.JobOutcome{JobID: job.ID, Kind: job.Kind, Key: job.IdempotencyKey},
	}
}
