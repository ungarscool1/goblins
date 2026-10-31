package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

// Route tells the inbox which project a source's container belongs to. Anything
// without a route is dropped: this is the allow-list for inbound events.
type Route struct {
	Source    string
	Container string
	Project   string
	Tracker   string
	SCM       string
}

// Inbox validates and stores normalized events. It never starts work directly: a
// stored event still has to pass through the workflow.
type Inbox struct {
	Store  core.StateStore
	Log    *slog.Logger
	Now    func() time.Time
	routes map[string]Route
}

// NewInbox builds an inbox from its routes.
func NewInbox(store core.StateStore, log *slog.Logger, routes []Route) *Inbox {
	in := &Inbox{Store: store, Log: log, routes: map[string]Route{}}
	for _, r := range routes {
		in.routes[r.Source+"|"+r.Container] = r
	}
	return in
}

// Allowed reports whether a source may deliver events about a container.
func (in *Inbox) Allowed(source, container string) bool {
	_, ok := in.routes[source+"|"+container]
	return ok
}

// Accept stores events, returning how many were new. Events for unrouted
// containers are dropped and logged.
func (in *Inbox) Accept(ctx context.Context, source string, events []core.Event) (int, error) {
	inserted := 0
	for _, ev := range events {
		route, ok := in.routes[source+"|"+ev.Subject.Container]
		if !ok {
			in.Log.Warn("event for an unrouted container dropped", "source", source, "container", ev.Subject.Container)
			continue
		}
		ev.Source = source
		ev.Project = route.Project
		ev.Subject.Provider = route.Tracker
		if ev.Subject.Kind == core.SubjectPullRequest {
			ev.Subject.Provider = route.SCM
		}
		if ev.ReceivedAt.IsZero() {
			ev.ReceivedAt = in.now()
		}
		_, isNew, err := in.Store.AppendEvent(ctx, ev)
		if err != nil {
			return inserted, err
		}
		if isNew {
			inserted++
		}
	}
	return inserted, nil
}

func (in *Inbox) now() time.Time {
	if in.Now != nil {
		return in.Now()
	}
	return time.Now().UTC()
}

// PollSource is reconciliation for one source and container. It covers deployments
// without inbound connectivity, and webhooks lost while down.
type PollSource struct {
	Source    core.EventSource
	Container string
	Interval  time.Duration
	// Lookback is how far back the first poll reads.
	Lookback time.Duration
}

func (s *Scheduler) poll(ctx context.Context) {
	for _, p := range s.Sources {
		name := p.Source.Info().Name
		key := "poll:" + name + ":" + p.Container
		s.mu.Lock()
		due := s.Now().Sub(s.last[key]) >= p.Interval
		if due {
			s.last[key] = s.Now()
		}
		s.mu.Unlock()
		if due {
			s.pollOne(ctx, p, name, key)
		}
	}
}

func (s *Scheduler) pollOne(ctx context.Context, p PollSource, name, key string) {
	cursor, err := s.Store.GetCursor(ctx, key)
	if err != nil {
		s.Log.Warn("cannot read cursor", "source", name, "err", err)
		return
	}
	res, err := p.Source.Poll(ctx, core.PollRequest{Container: p.Container, Cursor: cursor, Since: s.Now().Add(-p.Lookback)})
	if err != nil {
		s.Log.Warn("poll failed", "source", name, "container", p.Container, "err", err)
		return
	}
	// Events first, cursor second: a crash in between re-reads, and dedup absorbs it.
	if _, err := s.Inbox.Accept(ctx, name, res.Events); err != nil {
		s.Log.Warn("cannot store polled events", "source", name, "err", err)
		return
	}
	if res.NextCursor != "" && res.NextCursor != cursor {
		if err := s.Store.SetCursor(ctx, key, res.NextCursor); err != nil {
			s.Log.Warn("cannot store cursor", "source", name, "err", err)
		}
	}
}
