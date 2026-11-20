package app

import (
	"fmt"
	"slices"
	"time"

	"github.com/ungarscool1/goblins/internal/config"
	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers"
	"github.com/ungarscool1/goblins/internal/scheduler"
)

// containers are what a project's events can be about: its repository and, when the tracker
// keeps tickets somewhere else, such as a Jira project, that place too.
func containers(pc config.Project) []string {
	out := []string{pc.Repository}
	if tc := pc.TrackerContainer; tc != "" && tc != pc.Repository {
		out = append(out, tc)
	}
	return out
}

// projectRoutes routes each event source of a project to the containers it serves, and polls
// them when the project asks. A source that says which containers it serves is only given
// those, so a repository is never polled through a tracker's source or the reverse.
func projectRoutes(b *builder, cfg config.Config, pc config.Project,
	sys *System) ([]scheduler.Route, []scheduler.PollSource, error) {
	var routes []scheduler.Route
	var polls []scheduler.PollSource
	for _, name := range pc.EventSource {
		src, err := build[core.EventSource](b, providers.KindEventSource, cfg.EventSources, name)
		if err != nil {
			return nil, nil, err
		}
		if src.Info().Has(core.CapEventsWebhook) {
			sys.Webhooks[name] = src
		}
		canPoll := src.Info().Has(core.CapEventsPoll)
		if pc.PollInterval > 0 && !canPoll {
			sys.Warnings = append(sys.Warnings, fmt.Sprintf("project %s: event source %s cannot "+
				"poll, so poll_interval has no effect", pc.ID, name))
		}
		served := 0
		for _, c := range containers(pc) {
			if scoped, ok := src.(core.ContainerScoped); ok && !scoped.Serves(c) {
				continue
			}
			served++
			routes = append(routes, scheduler.Route{Source: name, Container: c, Project: pc.ID,
				Tracker: pc.Tracker, SCM: pc.SCM})
			if pc.PollInterval > 0 && canPoll {
				polls = append(polls, scheduler.PollSource{Source: src, Container: c,
					Interval: pc.PollInterval, Lookback: 24 * time.Hour})
			}
		}
		if served == 0 {
			sys.Warnings = append(sys.Warnings, fmt.Sprintf("project %s: event source %s serves "+
				"none of %v, so it reports nothing for this project", pc.ID, name, containers(pc)))
		}
	}
	return routes, polls, nil
}

// checkAgentBoundary refuses a project whose tracker restricts which agents may see its
// tickets, when one of its stages uses another. It is checked before anything runs.
func checkAgentBoundary(pc config.Project, tracker core.IssueTracker) error {
	b, ok := tracker.(core.AgentBoundary)
	if !ok || len(b.AllowedAgents()) == 0 {
		return nil
	}
	allowed := b.AllowedAgents()
	for _, a := range stageAgents(pc) {
		if !slices.Contains(allowed, a) {
			return core.Errorf(core.KindInvalid, "app.agent_boundary",
				"project %s: tracker %s only allows agents %v, and a stage uses %s", pc.ID,
				pc.Tracker, allowed, a)
		}
	}
	return nil
}

func stageAgents(pc config.Project) []string {
	s := pc.Stages
	out := []string{s.Planner.Agent, s.Implementer.Agent}
	for _, st := range []*config.Stage{s.Describer, s.Triager} {
		if st != nil {
			out = append(out, st.Agent)
		}
	}
	for _, r := range s.Reviewers {
		out = append(out, r.Agent)
	}
	return out
}
