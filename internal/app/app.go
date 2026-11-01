// Package app wires configuration, providers and services into a running process. It is
// the composition root and the only place that sees both contracts and concrete adapters;
// business logic lives elsewhere.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/ungarscool1/goblins/internal/api"
	"github.com/ungarscool1/goblins/internal/config"
	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/pipeline"
	"github.com/ungarscool1/goblins/internal/providers"
	"github.com/ungarscool1/goblins/internal/providers/fake"
	"github.com/ungarscool1/goblins/internal/runtime/workspace"
	"github.com/ungarscool1/goblins/internal/scheduler"
	"github.com/ungarscool1/goblins/internal/storage"
	"github.com/ungarscool1/goblins/internal/workflow"
)

// Version is overridden at build time with -ldflags.
var Version = "dev"

// Options are the process-level inputs beyond configuration.
type Options struct {
	Registry *providers.Registry
	// Mock, when set, adds the routes that drive the fake world by hand.
	Mock *fake.World
	Now  func() time.Time
}

// System is a fully wired installation.
type System struct {
	Store     core.StateStore
	Scheduler *scheduler.Scheduler
	Inbox     *scheduler.Inbox
	Webhooks  map[string]core.EventSource
	Projects  map[string]*pipeline.Project
	Agents    map[string]core.AgentProvider
	Runtimes  map[string]core.ExecutionRuntime
	Warnings  []string
}

// OpenStore opens the configured database.
func OpenStore(ctx context.Context, cfg config.Config) (core.StateStore, error) {
	switch cfg.Storage.Driver {
	case "postgres":
		dsn, ok := os.LookupEnv(cfg.Storage.DSNEnv)
		if !ok || dsn == "" {
			return nil, core.Errorf(core.KindInvalid, "app.storage", "environment variable %s is not set", cfg.Storage.DSNEnv)
		}
		return storage.OpenPostgres(ctx, dsn)
	default:
		file := cfg.Storage.Path
		if file == "" {
			file = filepath.Join(cfg.Installation.DataDir, "goblins.db")
		}
		return storage.OpenSQLite(ctx, file)
	}
}

type builder struct {
	cfg  config.Config
	reg  *providers.Registry
	deps providers.Deps
	seen map[string]core.Provider
}

func build[T core.Provider](b *builder, kind providers.Kind, set config.ProviderSet, name string) (T, error) {
	var zero T
	key := string(kind) + "/" + name
	if p, ok := b.seen[key]; ok {
		t, ok := p.(T)
		if !ok {
			return zero, core.Errorf(core.KindInternal, "app.build", "%s does not implement its contract", key)
		}
		return t, nil
	}
	def := set[name]
	t, err := providers.As[T](b.reg, kind, def.Type, name, b.deps, providers.Options(def.Options))
	if err != nil {
		return zero, fmt.Errorf("%s %q: %w", kind, name, err)
	}
	b.seen[key] = t
	return t, nil
}

// Build constructs all providers and services. It starts nothing.
func Build(cfg config.Config, store core.StateStore, log *slog.Logger, o Options) (*System, error) {
	now := o.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	b := &builder{cfg: cfg, reg: o.Registry, seen: map[string]core.Provider{},
		deps: providers.Deps{Log: log, Installation: cfg.Installation.Name, DataDir: cfg.Installation.DataDir, Getenv: os.LookupEnv, Now: now}}
	sys := &System{Store: store, Webhooks: map[string]core.EventSource{}, Projects: map[string]*pipeline.Project{}, Agents: map[string]core.AgentProvider{}, Runtimes: map[string]core.ExecutionRuntime{}}

	policies := map[string]workflow.Policy{}
	var routes []scheduler.Route
	var polls []scheduler.PollSource
	limits := map[string]int{}

	stage := func(s config.Stage) (pipeline.Stage, error) {
		agent, err := build[core.AgentProvider](b, providers.KindAgent, cfg.Agents, s.Agent)
		if err != nil {
			return pipeline.Stage{}, err
		}
		runtime, err := build[core.ExecutionRuntime](b, providers.KindRuntime, cfg.Runtimes, s.Runtime)
		if err != nil {
			return pipeline.Stage{}, err
		}
		sys.Agents[s.Agent] = agent
		sys.Runtimes[s.Runtime] = runtime
		if m := cfg.Agents[s.Agent].MaxConcurrent; m > 0 {
			limits[limitKey(s.Agent)] = m
		}
		return pipeline.Stage{Agent: agent, Runtime: runtime, Model: s.Model, Effort: s.Effort, Timeout: s.Timeout}, nil
	}

	for _, pc := range cfg.Projects {
		pr := &pipeline.Project{ID: pc.ID, Repo: core.RepoRef{Slug: pc.Repository}, BaseBranch: pc.BaseBranch, GitImage: pc.Workspace.GitImage,
			Identity:    workspace.Identity{Name: pc.Workspace.CommitName, Email: pc.Workspace.CommitEmail, Signoff: pc.Workspace.Signoff},
			WebResearch: pc.Policy.WebResearch, AllowUnvalidated: pc.Policy.AllowUnvalidatedDraftPR, AllowSkippedReview: pc.Policy.AllowSkippedReview}
		if pr.BaseBranch == "" {
			pr.BaseBranch = "main"
		}
		if pr.Identity.Name == "" {
			pr.Identity.Name = "Goblins"
		}
		if pr.Identity.Email == "" {
			pr.Identity.Email = "goblins@localhost.invalid"
		}
		var err error
		if pr.Tracker, err = build[core.IssueTracker](b, providers.KindTracker, cfg.Trackers, pc.Tracker); err != nil {
			return nil, err
		}
		if pr.SCM, err = build[core.SourceControl](b, providers.KindSCM, cfg.SCMs, pc.SCM); err != nil {
			return nil, err
		}
		if pr.Planner, err = stage(pc.Stages.Planner); err != nil {
			return nil, err
		}
		if pr.Implementer, err = stage(pc.Stages.Implementer); err != nil {
			return nil, err
		}
		if pc.Validation != "" {
			if pr.Validation, err = build[core.ValidationProvider](b, providers.KindValidation, cfg.Validations, pc.Validation); err != nil {
				return nil, err
			}
		}
		if pc.Review != "" {
			if pr.Review, err = build[core.ReviewProvider](b, providers.KindReview, cfg.Reviews, pc.Review); err != nil {
				return nil, err
			}
		}
		for i, s := range pc.Stages.Reviewers {
			st, err := stage(s)
			if err != nil {
				return nil, err
			}
			pr.ReviewFallbacks = append(pr.ReviewFallbacks,
				&pipeline.AgentReviewer{Name: fmt.Sprintf("%s-%s-%d", pc.ID, s.Agent, i),
					Stage: st})
		}
		if pc.Notification != "" {
			if pr.Notification, err = build[core.NotificationProvider](b, providers.KindNotification, cfg.Notifications, pc.Notification); err != nil {
				return nil, err
			}
		}
		policy := policyFor(pc)
		pr.MinInput, pr.MinApprove = policy.MinInputPermission, policy.MinApprovePermission
		pr.TrustedReviewBots = policy.TrustedReviewBots
		sys.Projects[pc.ID] = pr
		policies[pc.ID] = policy
		sys.Warnings = append(sys.Warnings, preflight(pc, pr)...)

		for _, name := range pc.EventSource {
			src, err := build[core.EventSource](b, providers.KindEventSource, cfg.EventSources, name)
			if err != nil {
				return nil, err
			}
			routes = append(routes, scheduler.Route{Source: name, Container: pc.Repository, Project: pc.ID, Tracker: pc.Tracker, SCM: pc.SCM})
			if src.Info().Has(core.CapEventsWebhook) {
				sys.Webhooks[name] = src
			}
			if pc.PollInterval > 0 {
				if !src.Info().Has(core.CapEventsPoll) {
					sys.Warnings = append(sys.Warnings, fmt.Sprintf("project %s: event source %s cannot poll, so poll_interval has no effect", pc.ID, name))
					continue
				}
				polls = append(polls, scheduler.PollSource{Source: src, Container: pc.Repository, Interval: pc.PollInterval, Lookback: 24 * time.Hour})
			}
		}
	}

	sys.Inbox = scheduler.NewInbox(store, log, routes)
	pipe := &pipeline.Pipeline{Installation: cfg.Installation.Name, Projects: sys.Projects, Designs: store, Log: log}
	hostname, _ := os.Hostname()
	owner := cfg.Installation.Name + "/" + hostname + "/" + strconv.Itoa(os.Getpid())
	engine := &workflow.Engine{Store: store, Owner: owner, Log: log, Now: now, Auth: authorizer{projects: sys.Projects},
		Policies: func(p string) (workflow.Policy, bool) { v, ok := policies[p]; return v, ok }}
	var rts []core.ExecutionRuntime
	for _, name := range slices.Sorted(maps.Keys(sys.Runtimes)) {
		rts = append(rts, sys.Runtimes[name])
	}
	sys.Scheduler = &scheduler.Scheduler{Store: store, Engine: engine, Handlers: pipe.Handlers(), Sources: polls, Inbox: sys.Inbox, Runtimes: rts, Log: log, Now: now,
		Cfg: scheduler.Config{Owner: owner, Installation: cfg.Installation.Name, Workers: cfg.Scheduler.Workers,
			Limits: scheduler.Limits{MaxGlobal: cfg.Scheduler.MaxGlobal, MaxPerProject: cfg.Scheduler.MaxPerProject, MaxPerKey: limits}}}
	return sys, nil
}

func limitKey(agent string) string { return "agent:" + agent }

func policyFor(pc config.Project) workflow.Policy {
	p := workflow.DefaultPolicy()
	c := pc.Policy
	if c.ApproveCommand != "" {
		p.ApproveCommand = c.ApproveCommand
	}
	if c.MinApprovePermission != "" {
		p.MinApprovePermission = core.ParsePermission(c.MinApprovePermission)
	}
	if c.MinInputPermission != "" {
		p.MinInputPermission = core.ParsePermission(c.MinInputPermission)
	}
	p.IntakeLabel = c.IntakeLabel
	p.ReviewPullRequests = c.ReviewPullRequests
	p.BotLogins = c.BotLogins
	if c.MaxPlanLoops > 0 {
		p.MaxPlanLoops = c.MaxPlanLoops
	}
	if c.MaxImplementLoops > 0 {
		p.MaxImplementLoops = c.MaxImplementLoops
	}
	if c.QuotaPause > 0 {
		p.QuotaPause = c.QuotaPause
	}
	if c.AuthPause > 0 {
		p.AuthPause = c.AuthPause
	}
	if c.MaxReviewFixLoops > 0 {
		p.MaxReviewFixLoops = c.MaxReviewFixLoops
	}
	if c.ReviewMinSeverity != "" {
		p.ReviewMinSeverity = core.ReviewSeverity(c.ReviewMinSeverity)
	}
	p.StopAtReport = c.StopAtReport
	p.TrustedReviewBots = c.TrustedReviewBots
	p.Merge = mergePolicy(c.Merge)
	p.SkipReviewTriage = c.SkipReviewTriage
	if c.MaxDebateRounds > 0 {
		p.MaxDebateRounds = c.MaxDebateRounds
	}
	if c.MaxTriageRounds > 0 {
		p.MaxTriageRounds = c.MaxTriageRounds
	}
	if c.ReplyTimeout > 0 {
		p.ReplyTimeout = c.ReplyTimeout
	}
	if len(pc.Stages.Reviewers) > 0 {
		p.ReviewerLimitKey = limitKey(pc.Stages.Reviewers[0].Agent)
	}
	p.PlannerLimitKey = limitKey(pc.Stages.Planner.Agent)
	p.ImplementerLimitKey = limitKey(pc.Stages.Implementer.Agent)
	return p
}

func mergePolicy(c config.MergePolicy) workflow.MergePolicy {
	m := workflow.DefaultMergePolicy()
	m.RequiredChecks, m.Approvers = c.RequiredChecks, c.Approvers
	m.RequireBranchProtection = c.RequireBranchProtection
	if c.MinHumanApprovals > 0 {
		m.MinHumanApprovals = c.MinHumanApprovals
	}
	for category, paths := range c.SensitivePaths {
		m.SensitivePaths[category] = append(m.SensitivePaths[category], paths...)
	}
	for _, r := range c.AutoMerge {
		rule := workflow.AutoMergeRule{Name: r.Name, Paths: r.Paths, MaxFiles: r.MaxFiles}
		m.AutoMerge = append(m.AutoMerge, rule)
	}
	return m
}

// preflight reports configurations that would run but cannot work as expected. Missing
// capabilities still surface as blocked tasks; this tells the operator ahead of time.
func preflight(pc config.Project, pr *pipeline.Project) []string {
	var out []string
	warn := func(p core.Provider, what string, caps ...core.Capability) {
		if err := core.Negotiate(p, caps...); err != nil {
			out = append(out, fmt.Sprintf("project %s: %s %s", pc.ID, core.UserMessage(err), what))
		}
	}
	warn(pr.Tracker, "(nobody can approve designs without it)", core.CapTrackerPermissions)
	if pr.Notification == nil {
		warn(pr.Tracker, "(no notification provider is configured as a fallback)", core.CapTrackerCommentWrite)
	}
	warn(pr.SCM, "(pull requests cannot be opened)", core.CapSCMPullRequests, core.CapSCMDraft)
	if !pc.Policy.SkipReviewTriage && !pc.Policy.StopAtReport {
		warn(pr.SCM, "(review conversations block for a person)", core.CapSCMThreadReply)
	}
	warn(pr.Implementer.Runtime, "(workspaces cannot be provided)", core.CapRuntimeVolumes, core.CapRuntimeNetworkNone)
	warn(pr.Planner.Agent, "(sessions restart from scratch)", core.CapAgentResume)
	if pr.Validation == nil {
		out = append(out, fmt.Sprintf("project %s: no validation is configured, so pull requests will be opened untested", pc.ID))
	}
	return out
}

type authorizer struct{ projects map[string]*pipeline.Project }

func (a authorizer) Permission(ctx context.Context, project string, s core.Subject, actor core.Actor) (core.Permission, error) {
	pr, ok := a.projects[project]
	if !ok {
		return core.PermissionNone, core.Errorf(core.KindInvalid, "app.authorize", "unknown project")
	}
	return pr.Tracker.ActorPermission(ctx, core.IssueRef{Container: s.Container, ExternalID: s.ExternalID}, actor)
}

// Run starts the service and blocks until ctx is cancelled and shutdown has ended.
func Run(ctx context.Context, cfg config.Config, log *slog.Logger, o Options) error {
	if err := checkDataDir(cfg.Installation.DataDir); err != nil {
		return err
	}
	if rendered, err := cfg.Render(); err == nil {
		log.Debug("effective configuration", "config", string(rendered))
	}
	store, err := OpenStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	sys, err := Build(cfg, store, log, o)
	if err != nil {
		return err
	}
	for _, w := range sys.Warnings {
		log.Warn("configuration warning", "detail", w)
	}

	srv := api.NewServer(log, Version, []api.Check{
		{Name: "data_dir", Run: func(context.Context) error { return checkDataDir(cfg.Installation.DataDir) }},
		{Name: "storage", Run: store.Ping},
	})
	srv.Handle("POST /webhooks/{source}", api.WebhookHandler(sys.Webhooks, sys.Inbox, log))
	if o.Mock != nil {
		mountMock(srv, sys, cfg, o.Mock, log)
	}

	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		return core.Wrap(core.KindInvalid, "app.listen", err)
	}
	log.Info("goblins started", "version", Version, "installation", cfg.Installation.Name, "listen", ln.Addr().String(), "projects", len(cfg.Projects))

	loopCtx, stopLoop := context.WithCancel(ctx)
	defer stopLoop()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := sys.Scheduler.Run(loopCtx); err != nil {
			log.Error("scheduler stopped", "err", err)
		}
	}()

	srv.SetReady(true)
	serveErr := srv.Serve(ctx, ln, cfg.Server.ShutdownTimeout)
	stopLoop()
	wg.Wait()
	log.Info("goblins stopped")
	return serveErr
}

// checkDataDir verifies the data directory exists and is writable, creating it with
// owner-only permissions if needed.
func checkDataDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return core.Wrap(core.KindInvalid, "app.data_dir", err)
	}
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return core.Wrap(core.KindInvalid, "app.data_dir", fmt.Errorf("not writable: %w", err))
	}
	_ = f.Close()
	return os.Remove(filepath.Clean(f.Name()))
}

// OperatorComment records a comment from someone with shell access to this installation.
// It is the manual inbox for trackers that cannot carry a conversation: the same rules
// apply to its content, and the shell access itself is the proof of permission.
func OperatorComment(ctx context.Context, store core.StateStore, taskID, login, body string, now time.Time) error {
	task, err := store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	actor := core.Actor{ID: "operator:" + login, Login: login, Type: core.ActorHuman}
	id := core.Digest(taskID, login, body, now.String())[:24]
	_, _, err = store.AppendEvent(ctx, core.Event{
		Project: task.Project, Source: workflow.SourceOperator, Type: core.EventCommentCreated, Subject: task.Subject,
		DedupKey: "operator:" + id, Actor: actor, OccurredAt: now, ReceivedAt: now,
		Comment: &core.Comment{ID: "operator-" + id, Author: actor, Body: body, CreatedAt: now, UpdatedAt: now},
	})
	return err
}

// mountMock adds routes that act as the outside world in mock mode. Without --mock they do
// not exist. They are for trying out the workflow by hand.
func mountMock(srv *api.Server, sys *System, cfg config.Config, w *fake.World, log *slog.Logger) {
	const operator = "operator"
	human := core.Actor{ID: "1", Login: operator, Type: core.ActorHuman}
	for _, p := range cfg.Projects {
		w.SetPermission(p.Repository, operator, core.PermissionAdmin)
		w.SetBranch(p.Repository, "main", "1111111111111111111111111111111111111111")
	}
	route := func(project string) (config.Project, bool) {
		for _, p := range cfg.Projects {
			if p.ID == project {
				return p, true
			}
		}
		return config.Project{}, false
	}
	accept := func(rw http.ResponseWriter, r *http.Request, p config.Project, ev core.Event) {
		if len(p.EventSource) == 0 {
			http.Error(rw, "project has no event source", http.StatusBadRequest)
			return
		}
		if _, err := sys.Inbox.Accept(r.Context(), p.EventSource[0], []core.Event{ev}); err != nil {
			log.Error("mock event not stored", "err", err)
			http.Error(rw, "not stored", http.StatusServiceUnavailable)
		}
	}
	srv.Handle("POST /mock/{project}/issues", http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		p, ok := route(r.PathValue("project"))
		if !ok {
			http.NotFound(rw, r)
			return
		}
		var in struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 1<<20)).Decode(&in); err != nil {
			http.Error(rw, "invalid JSON", http.StatusBadRequest)
			return
		}
		iss := w.AddIssue(p.Repository, in.Title, in.Body, human)
		accept(rw, r, p, core.Event{Type: core.EventIssueOpened, DedupKey: "mock:issue:" + iss.Ref.ExternalID, Actor: human, OccurredAt: iss.CreatedAt, Issue: &iss,
			Subject: core.Subject{Kind: core.SubjectIssue, Container: p.Repository, ExternalID: iss.Ref.ExternalID}})
		_ = json.NewEncoder(rw).Encode(map[string]string{"issue": iss.Ref.ExternalID})
	}))
	srv.Handle("POST /mock/{project}/issues/{issue}/comments", http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		p, ok := route(r.PathValue("project"))
		if !ok {
			http.NotFound(rw, r)
			return
		}
		var in struct {
			Body string `json:"body"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 1<<20)).Decode(&in); err != nil {
			http.Error(rw, "invalid JSON", http.StatusBadRequest)
			return
		}
		ref := core.IssueRef{Container: p.Repository, ExternalID: r.PathValue("issue")}
		c := w.AddComment(ref, human, in.Body)
		accept(rw, r, p, core.Event{Type: core.EventCommentCreated, DedupKey: "mock:comment:" + c.ID, Actor: human, OccurredAt: c.CreatedAt, Comment: &c,
			Subject: core.Subject{Kind: core.SubjectIssue, Container: p.Repository, ExternalID: ref.ExternalID}})
		_ = json.NewEncoder(rw).Encode(c)
	}))
	srv.Handle("GET /mock/{project}/issues/{issue}/comments", http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		p, ok := route(r.PathValue("project"))
		if !ok {
			http.NotFound(rw, r)
			return
		}
		_ = json.NewEncoder(rw).Encode(w.Comments(core.IssueRef{Container: p.Repository, ExternalID: r.PathValue("issue")}))
	}))
}

// Doctor checks an installation without starting it or using any model.
func Doctor(ctx context.Context, sys *System, out func(format string, args ...any)) error {
	var problems []error
	for _, name := range slices.Sorted(maps.Keys(sys.Projects)) {
		pr := sys.Projects[name]
		out("project %s: tracker=%s scm=%s agent=%s runtime=%s\n", name, pr.Tracker.Info().Type, pr.SCM.Info().Type, pr.Implementer.Agent.Info().Type, pr.Implementer.Runtime.Info().Type)
		if _, err := pr.SCM.ResolveBranch(ctx, pr.Repo, pr.BaseBranch); err != nil {
			problems = append(problems, fmt.Errorf("project %s: base branch %s: %s", name, pr.BaseBranch, core.UserMessage(err)))
		}
		for _, role := range []pipeline.Stage{pr.Planner, pr.Implementer} {
			st, err := role.Agent.AuthStatus(ctx, role.Runtime)
			if err != nil {
				problems = append(problems, fmt.Errorf("agent %s: %s", role.Agent.Info().Name, core.UserMessage(err)))
				continue
			}
			out("agent %s: %s %s\n", role.Agent.Info().Name, st.State, st.Detail)
			if st.State != core.AuthOK {
				problems = append(problems, fmt.Errorf("agent %s: %s", role.Agent.Info().Name, st.Detail))
			}
		}
		// A reviewer that cannot run is not a problem: the next one takes over.
		for _, r := range pr.ReviewFallbacks {
			if a, ok := r.(*pipeline.AgentReviewer); ok {
				st, err := a.Stage.Agent.AuthStatus(ctx, a.Stage.Runtime)
				if err != nil {
					out("reviewer %s: unavailable, review falls back: %s\n",
						a.Stage.Agent.Info().Name, core.UserMessage(err))
				} else {
					out("reviewer %s: %s %s\n", a.Stage.Agent.Info().Name, st.State, st.Detail)
				}
			}
		}
	}
	for _, w := range sys.Warnings {
		out("warning: %s\n", w)
	}
	return errors.Join(problems...)
}
