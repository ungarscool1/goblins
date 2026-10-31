// Package scheduler runs the control loop: it turns inbound signals into stored
// events, lets the workflow engine process them, and dispatches the resulting
// jobs to handlers under leases and concurrency limits. It knows contracts only,
// so it works unchanged with any tracker, agent or runtime.
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/observability"
	"github.com/ungarscool1/goblins/internal/workflow"
)

// Labels attached to every workload, used to reconcile orphans.
const (
	LabelInstallation = "goblins.installation"
	LabelJob          = "goblins.job"
	LabelAttempt      = "goblins.attempt"
	LabelTask         = "goblins.task"
)

// Limits bound how much work runs at once, so a burst of issues cannot exhaust a
// subscription. They apply to heavy jobs, meaning those with a limit key.
type Limits struct {
	MaxGlobal     int
	MaxPerProject int
	MaxPerKey     map[string]int
}

// Config tunes the scheduler. Zero values take the defaults in fill.
type Config struct {
	Owner        string
	Installation string
	Workers      int
	Limits       Limits
	Lease        time.Duration
	Heartbeat    time.Duration
	TickInterval time.Duration
	// JobTimeout caps a job's run time per kind; DefaultJobTimeout covers the rest.
	// This is the stuck-job detector for work that still heartbeats.
	JobTimeout        map[core.JobKind]time.Duration
	DefaultJobTimeout time.Duration
	RetryBase         time.Duration
	RetryMax          time.Duration
	ReconcileEvery    time.Duration
	MaxTranscript     int
	MaxTranscriptLine int
}

func (c *Config) fill() {
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	if c.Workers == 0 {
		c.Workers = 8
	}
	if c.Owner == "" {
		c.Owner = "scheduler"
	}
	def(&c.Lease, 2*time.Minute)
	def(&c.Heartbeat, 30*time.Second)
	def(&c.TickInterval, 500*time.Millisecond)
	def(&c.DefaultJobTimeout, 2*time.Hour)
	def(&c.RetryBase, 15*time.Second)
	def(&c.RetryMax, 15*time.Minute)
	def(&c.ReconcileEvery, 5*time.Minute)
	if c.MaxTranscript == 0 {
		c.MaxTranscript = 5000
	}
	if c.MaxTranscriptLine == 0 {
		c.MaxTranscriptLine = 8 * 1024
	}
}

// JobContext is what a handler gets.
type JobContext struct {
	Job  core.Job
	Task core.Task
	// SaveCheckpoint durably records progress, such as an agent session ID, so a
	// later attempt can continue instead of starting over.
	SaveCheckpoint func(ctx context.Context, v any) error
	// Log appends bounded, redacted output to the task's transcript.
	Log func(kind, text string)
}

// Handler performs one kind of job. Its result becomes the job outcome. It must
// be safe to run again: an attempt can be interrupted at any point.
type Handler func(ctx context.Context, jc JobContext) (any, error)

// Scheduler is the control loop.
type Scheduler struct {
	Store    core.StateStore
	Engine   *workflow.Engine
	Handlers map[core.JobKind]Handler
	Sources  []PollSource
	Inbox    *Inbox
	Runtimes []core.ExecutionRuntime
	Log      *slog.Logger
	Now      func() time.Time
	Cfg      Config

	once sync.Once
	wg   sync.WaitGroup
	sem  chan struct{}
	last map[string]time.Time
	mu   sync.Mutex
}

func (s *Scheduler) init() {
	s.once.Do(func() {
		s.Cfg.fill()
		s.sem = make(chan struct{}, s.Cfg.Workers)
		s.last = map[string]time.Time{}
		if s.Now == nil {
			s.Now = func() time.Time { return time.Now().UTC() }
		}
	})
}

// Run loops until ctx is cancelled, then waits for in-flight jobs to return.
// Interrupted jobs go back in the queue without losing their attempt budget.
func (s *Scheduler) Run(ctx context.Context) error {
	s.init()
	s.ReconcileWorkloads(ctx)
	ticker := time.NewTicker(s.Cfg.TickInterval)
	defer ticker.Stop()
	for {
		s.pass(ctx)
		select {
		case <-ctx.Done():
			s.wg.Wait()
			return nil
		case <-ticker.C:
		}
	}
}

// Step runs one pass and waits for all the work it started. Deterministic tests
// use it instead of Run.
func (s *Scheduler) Step(ctx context.Context) {
	s.init()
	for range 100 {
		before := s.pass(ctx)
		s.wg.Wait()
		if !before {
			return
		}
	}
}

// pass performs every housekeeping duty once; it reports whether anything ran.
func (s *Scheduler) pass(ctx context.Context) bool {
	s.fireTimers(ctx)
	s.poll(ctx)
	s.maybeReconcile(ctx)
	more := s.drainEvents(ctx)
	return s.dispatch(ctx) || more
}

func (s *Scheduler) drainEvents(ctx context.Context) bool {
	worked := false
	for ctx.Err() == nil {
		more, err := s.Engine.ProcessNext(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			s.Log.Warn("event processing failed", "err", err)
		}
		if !more {
			break
		}
		worked = true
	}
	return worked
}

func (s *Scheduler) fireTimers(ctx context.Context) {
	due, err := s.Store.DueTasks(ctx, s.Now())
	if err != nil {
		s.Log.Warn("cannot list paused tasks", "err", err)
		return
	}
	for _, t := range due {
		// The version in the key lets each pause fire once.
		ev := core.Event{
			Project: t.Project, Source: "engine", Type: core.EventTimerFired, Subject: t.Subject,
			DedupKey: "timer:" + t.ID + ":" + strconv.FormatInt(t.Version, 10), OccurredAt: s.Now(), ReceivedAt: s.Now(),
		}
		if _, _, err := s.Store.AppendEvent(ctx, ev); err != nil {
			s.Log.Warn("cannot fire timer", "task", t.ID, "err", err)
		}
	}
}

func (s *Scheduler) dispatch(ctx context.Context) bool {
	started := false
	for ctx.Err() == nil {
		select {
		case s.sem <- struct{}{}:
		default:
			return started
		}
		job, err := s.Store.ClaimJob(ctx, core.JobClaim{
			Owner: s.Cfg.Owner, LeaseFor: s.Cfg.Lease, Now: s.Now(),
			MaxGlobal: s.Cfg.Limits.MaxGlobal, MaxPerProject: s.Cfg.Limits.MaxPerProject, MaxPerLimit: s.Cfg.Limits.MaxPerKey,
		})
		if err != nil || job == nil {
			<-s.sem
			if err != nil && !errors.Is(err, context.Canceled) {
				s.Log.Warn("cannot claim a job", "err", err)
			}
			return started
		}
		started = true
		s.wg.Add(1)
		go func(j core.Job) {
			defer s.wg.Done()
			defer func() { <-s.sem }()
			s.runJob(ctx, j)
		}(*job)
	}
	return started
}

// runJob owns one attempt: heartbeat, run, then record exactly one result.
func (s *Scheduler) runJob(parent context.Context, job core.Job) {
	log := s.Log.With("job", job.ID, "kind", job.Kind, "attempt", job.Attempt, "task", job.TaskID)
	// Results are written with a context that survives shutdown: a finished job's
	// outcome should not be lost because the process is stopping.
	bg := context.WithoutCancel(parent)

	task, err := s.Store.GetTask(parent, job.TaskID)
	if err != nil {
		log.Warn("cannot load task", "err", err)
		if rerr := s.Store.RetryJob(bg, job.ID, job.LeaseOwner, job.Attempt, s.Now().Add(s.Cfg.RetryBase), "task unavailable"); rerr != nil {
			log.Warn("cannot requeue", "err", rerr)
		}
		return
	}
	if workflow.EmitsStart(job.Kind) {
		if _, _, err := s.Store.AppendEvent(parent, workflow.JobStartedEvent(task, job, s.Now())); err != nil {
			log.Warn("cannot record job start", "err", err)
		}
	}

	timeout := s.Cfg.DefaultJobTimeout
	if d, ok := s.Cfg.JobTimeout[job.Kind]; ok {
		timeout = d
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	lost := make(chan struct{})
	stopBeat := make(chan struct{})
	var beat sync.WaitGroup
	beat.Add(1)
	go func() {
		defer beat.Done()
		t := time.NewTicker(s.Cfg.Heartbeat)
		defer t.Stop()
		for {
			select {
			case <-stopBeat:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				err := s.Store.HeartbeatJob(ctx, job.ID, job.LeaseOwner, job.Attempt, s.Now().Add(s.Cfg.Lease))
				if core.IsKind(err, core.KindConflict) {
					// Cancelled, or another worker took over: this attempt must stop.
					close(lost)
					cancel()
					return
				}
				if err != nil {
					log.Warn("heartbeat failed", "err", err)
				}
			}
		}
	}()

	tr := s.newTranscript(job)
	result, runErr := s.invoke(ctx, job, task, tr)
	close(stopBeat)
	beat.Wait()
	tr.flush(bg)

	select {
	case <-lost:
		log.Info("attempt stopped: job was cancelled or taken over")
		return
	default:
	}
	s.record(bg, parent.Err() != nil, errors.Is(ctx.Err(), context.DeadlineExceeded), log, task, job, result, runErr)
}

func (s *Scheduler) invoke(ctx context.Context, job core.Job, task core.Task, tr *transcript) (res any, err error) {
	h, ok := s.Handlers[job.Kind]
	if !ok {
		return nil, core.Errorf(core.KindUnsupported, "scheduler", "no handler for job kind %s", job.Kind)
	}
	defer func() {
		if r := recover(); r != nil {
			s.Log.Error("job handler panicked", "job", job.ID, "kind", job.Kind, "panic", r)
			err = core.Errorf(core.KindInternal, "scheduler", "the job handler crashed")
		}
	}()
	return h(ctx, JobContext{
		Job: job, Task: task, Log: tr.log,
		SaveCheckpoint: func(ctx context.Context, v any) error {
			b, err := json.Marshal(v)
			if err != nil {
				return core.Wrap(core.KindInternal, "scheduler.checkpoint", err)
			}
			return s.Store.SaveCheckpoint(ctx, job.ID, job.LeaseOwner, job.Attempt, b)
		},
	})
}

func (s *Scheduler) record(bg context.Context, shuttingDown, timedOut bool, log *slog.Logger, task core.Task, job core.Job, result any, runErr error) {
	now := s.Now()
	finish := func(state core.JobState, o core.JobOutcome) {
		ev := workflow.JobOutcomeEvent(task, job, o, now)
		err := s.Store.FinishJob(bg, core.JobFinish{JobID: job.ID, Owner: job.LeaseOwner, Attempt: job.Attempt, State: state, Error: o.Message, Event: &ev, Now: now})
		if err != nil {
			log.Warn("cannot record job result", "err", err)
		}
	}
	if runErr == nil {
		raw, err := json.Marshal(result)
		if err != nil {
			finish(core.JobFailed, core.JobOutcome{ErrorKind: core.KindInternal, Message: "unreadable job result"})
			return
		}
		finish(core.JobSucceeded, core.JobOutcome{OK: true, Result: raw})
		return
	}

	// Shutting down is not the job's fault: run it again on the next start.
	if shuttingDown {
		if err := s.Store.RetryJob(bg, job.ID, job.LeaseOwner, job.Attempt, now, "interrupted by shutdown"); err != nil {
			log.Warn("cannot requeue interrupted job", "err", err)
		}
		return
	}
	kind := core.KindOf(runErr)
	message := observability.RedactText(core.UserMessage(runErr))
	if timedOut {
		kind, message = core.KindTimeout, "the job exceeded its time limit"
	}
	log.Warn("job attempt failed", "error_kind", kind, "err", runErr)
	retryable := kind == core.KindTransient || kind == core.KindTimeout || kind == core.KindInternal
	if retryable && job.Attempt < job.MaxAttempts {
		if err := s.Store.RetryJob(bg, job.ID, job.LeaseOwner, job.Attempt, now.Add(s.backoff(job.Attempt)), message); err != nil {
			log.Warn("cannot requeue job", "err", err)
		}
		return
	}
	finish(core.JobFailed, core.JobOutcome{ErrorKind: kind, Message: message})
}

func (s *Scheduler) backoff(attempt int) time.Duration {
	d := s.Cfg.RetryBase << min(attempt-1, 16)
	return min(d, s.Cfg.RetryMax)
}

func (s *Scheduler) maybeReconcile(ctx context.Context) {
	s.mu.Lock()
	due := s.Now().Sub(s.last["reconcile"]) >= s.Cfg.ReconcileEvery
	if due {
		s.last["reconcile"] = s.Now()
	}
	s.mu.Unlock()
	if due {
		s.ReconcileWorkloads(ctx)
	}
}

// ReconcileWorkloads removes workloads no live attempt owns: leftovers of crashes,
// lost leases and cancelled jobs. A workload whose attempt still holds a valid
// lease, possibly on another replica, is left alone.
func (s *Scheduler) ReconcileWorkloads(ctx context.Context) {
	s.init()
	for _, rt := range s.Runtimes {
		list, err := rt.List(ctx, map[string]string{LabelInstallation: s.Cfg.Installation})
		if err != nil {
			s.Log.Warn("cannot list workloads", "runtime", rt.Info().Name, "err", err)
			continue
		}
		for _, w := range list {
			s.reconcileOne(ctx, rt, w)
		}
	}
}

func (s *Scheduler) reconcileOne(ctx context.Context, rt core.ExecutionRuntime, w core.WorkloadStatus) {
	jobID := w.Labels[LabelJob]
	if jobID == "" {
		return
	}
	job, err := s.Store.GetJob(ctx, jobID)
	if err != nil && !core.IsKind(err, core.KindNotFound) {
		return
	}
	live := err == nil && job.State == core.JobRunning &&
		strconv.Itoa(job.Attempt) == w.Labels[LabelAttempt] && job.LeaseUntil.After(s.Now())
	if live {
		return
	}
	s.Log.Info("removing orphan workload", "workload", w.Name, "job", jobID)
	if err := rt.Cancel(ctx, w.Name); err != nil && !core.IsKind(err, core.KindNotFound) {
		s.Log.Warn("cannot cancel orphan", "workload", w.Name, "err", err)
	}
	if err := rt.Cleanup(ctx, w.Name); err != nil {
		s.Log.Warn("cannot remove orphan", "workload", w.Name, "err", err)
	}
}

type transcript struct {
	s     *Scheduler
	job   core.Job
	mu    sync.Mutex
	buf   []core.TranscriptEntry
	count int
}

func (s *Scheduler) newTranscript(job core.Job) *transcript { return &transcript{s: s, job: job} }

// log keeps output bounded and redacted before it reaches the database.
func (t *transcript) log(kind, text string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.count >= t.s.Cfg.MaxTranscript {
		return
	}
	t.count++
	if t.count == t.s.Cfg.MaxTranscript {
		kind, text = "system", "transcript limit reached, further output dropped"
	}
	if n := t.s.Cfg.MaxTranscriptLine; len(text) > n {
		text = text[:n] + " [truncated]"
	}
	t.buf = append(t.buf, core.TranscriptEntry{TaskID: t.job.TaskID, JobID: t.job.ID, Kind: kind, Text: observability.RedactText(text), At: t.s.Now()})
	if len(t.buf) >= 50 {
		t.flushLocked(context.Background())
	}
}

func (t *transcript) flush(ctx context.Context) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.flushLocked(ctx)
}

func (t *transcript) flushLocked(ctx context.Context) {
	if len(t.buf) == 0 {
		return
	}
	if err := t.s.Store.AppendTranscript(ctx, t.buf); err != nil {
		t.s.Log.Warn("cannot store transcript", "job", t.job.ID, "err", err)
	}
	t.buf = nil
}
