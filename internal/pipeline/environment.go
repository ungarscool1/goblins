package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/runtime/workspace"
	"github.com/ungarscool1/goblins/internal/scheduler"
)

// EnvironmentStore is what environments need: durable leases, and the tasks holding them.
type EnvironmentStore interface {
	core.LeaseStore
	GetTask(ctx context.Context, id string) (core.Task, error)
}

const (
	defaultEnvironmentLease = 2 * time.Hour
	busyRetry               = time.Minute
	// Lease notes. "running" names a step that is not idempotent while it runs, so an
	// interruption is seen; "broken" says a release failed and a person must look.
	noteAcquired = "acquired"
	noteRunning  = "running"
	noteBroken   = "broken"
)

// forever is how long a broken lease is kept: until a person releases it.
var forever = time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC) // within int64 nanoseconds

func leaseResource(provider, target string) string { return "env:" + provider + ":" + target }

// DirtyResource names the marker that a shared target was changed and not yet reset. It is kept
// apart from the target's lease, so that it outlives that lease expiring or being taken over:
// whoever holds the target next resets it first, however they came to hold it.
func DirtyResource(lease string) string { return "env-dirty:" + strings.TrimPrefix(lease, "env:") }

// ephemeralTarget names a task's own environment. It is derived, so a retry finds it again.
func ephemeralTarget(taskID string) string {
	sum := sha256.Sum256([]byte(taskID))
	return "goblins-" + hex.EncodeToString(sum[:])[:12]
}

type envRun struct {
	p      *Pipeline
	jc     scheduler.JobContext
	pr     *Project
	sha    string
	ws     core.Mount
	env    core.Environment
	lease  core.Lease
	checks []core.ValidationCheck
	n      int
}

// withEnvironment adds the environment's results to a report whose checks passed.
func (p *Pipeline) withEnvironment(ctx context.Context, jc scheduler.JobContext, pr *Project,
	sha, scratch string, report core.ValidationReport) (core.ValidationReport, error) {
	if report.Status != core.ValidationPassed || pr.Environment == nil {
		return report, nil
	}
	checks, failure, err := p.checkInEnvironment(ctx, jc, pr, sha, scratch)
	if err != nil {
		return report, err
	}
	report.Checks = append(report.Checks, checks...)
	if failure != "" {
		report.Status, report.Summary = core.ValidationFailed, failure
		if checks[len(checks)-1].Status == core.ValidationError {
			report.Status = core.ValidationError
		}
	}
	return report, nil
}

// checkInEnvironment deploys the tested commit into a development environment and verifies it
// there, after its checks passed. Goblins owns the order of steps and the lease; the provider
// only runs what it is asked. A busy environment is waited for; a broken one needs a person.
func (p *Pipeline) checkInEnvironment(ctx context.Context, jc scheduler.JobContext, pr *Project,
	sha, scratch string) ([]core.ValidationCheck, string, error) {
	r := &envRun{p: p, jc: jc, pr: pr, sha: sha,
		ws: core.Mount{Volume: scratch, Target: workspace.ScratchMount}}
	if err := r.acquire(ctx); err != nil {
		if core.IsKind(err, core.KindConflict) {
			return nil, "", err
		}
		msg := core.UserMessage(err)
		return []core.ValidationCheck{{Name: "environment", Stage: "environment",
			Status: core.ValidationError, Output: msg}}, msg, nil
	}
	if step := r.lease.Data[noteRunning]; step != "" {
		msg := fmt.Sprintf("step %s on %s was interrupted and cannot safely run twice. Check the "+
			"environment, then release it with `goblins environments --release %s`", step,
			r.env.Target, r.lease.Resource)
		return []core.ValidationCheck{{Name: step, Stage: "environment",
			Status: core.ValidationError, Output: msg}}, msg, nil
	}
	failed, err := r.deployAndVerify(ctx)
	if err != nil {
		return r.checks, "", err
	}
	if failed != "" {
		return r.checks, fmt.Sprintf("failed in environment %s: %s", r.env.Target, failed), nil
	}
	// Verified: the evidence is recorded, so the target goes back for others.
	if err := r.release(ctx); err != nil {
		return r.checks, "", err
	}
	return r.checks, "", nil
}

func (r *envRun) acquire(ctx context.Context) error {
	e := r.pr.Environment
	now := time.Now()
	ttl := r.pr.EnvironmentLease
	if ttl <= 0 {
		ttl = defaultEnvironmentLease
	}
	holder := r.jc.Task.ID
	targets := e.Targets()
	if len(targets) == 0 {
		target := ephemeralTarget(holder)
		r.env = core.Environment{Provider: e.Info().Name, Target: target, Ephemeral: true}
		l, err := r.p.Environments.AcquireLease(ctx, core.Lease{Resource: leaseResource(
			e.Info().Name, target), Holder: holder, AcquiredAt: now, ExpiresAt: now.Add(ttl)}, now)
		r.lease = l
		return err
	}
	broken := 0
	var holders []string
	for _, t := range targets {
		res := leaseResource(e.Info().Name, t)
		l, err := r.p.Environments.AcquireLease(ctx, core.Lease{Resource: res, Holder: holder,
			AcquiredAt: now, ExpiresAt: now.Add(ttl)}, now)
		if err == nil {
			r.env, r.lease = core.Environment{Provider: e.Info().Name, Target: t}, l
			return nil
		}
		if !core.IsKind(err, core.KindConflict) {
			return err
		}
		if l.Data[noteBroken] != "" {
			broken++
			continue
		}
		if r.reclaimable(ctx, l) {
			// Another task may win the target in between: the dirty marker makes whoever does
			// reset it first.
			if err := r.p.Environments.ReleaseLease(ctx, res, l.Holder); err != nil {
				return err
			}
			l2, err := r.p.Environments.AcquireLease(ctx, core.Lease{Resource: res, Holder: holder,
				AcquiredAt: now, ExpiresAt: now.Add(ttl)}, now)
			if err == nil {
				r.env, r.lease = core.Environment{Provider: e.Info().Name, Target: t}, l2
				return nil
			}
		}
		holders = append(holders, t+" (task "+l.Holder+")")
	}
	if broken == len(targets) {
		return core.Errorf(core.KindForbidden, "pipeline.environment",
			"every target of %s needs a "+
				"person: a reset failed. See `goblins environments`", e.Info().Name)
	}
	return core.WithRetryAt(core.Errorf(core.KindConflict, "pipeline.environment",
		"waiting for a target of %s: %s", e.Info().Name, strings.Join(holders, ", ")),
		now.Add(busyRetry))
}

// reclaimable says a lease's holder will never release it: its task is finished or gone.
func (r *envRun) reclaimable(ctx context.Context, l core.Lease) bool {
	t, err := r.p.Environments.GetTask(ctx, l.Holder)
	return core.IsKind(err, core.KindNotFound) || (err == nil && t.State.IsTerminal())
}

// deployAndVerify runs the steps in order and returns the first that failed, after gathering
// evidence. A shared target another task changed and did not reset is reset first; if that
// fails, it is kept from everyone until a person restores it.
func (r *envRun) deployAndVerify(ctx context.Context) (string, error) {
	if err := r.resetLeftOver(ctx); err != nil {
		return "", err
	}
	if r.lease.Data[noteAcquired] == "" {
		if failed, err := r.phase(ctx, core.PhaseAcquire); err != nil || failed != "" {
			return r.evidence(ctx, failed, err)
		}
		if err := r.note(ctx, noteAcquired, "true"); err != nil {
			return "", err
		}
	}
	for _, ph := range []core.EnvironmentPhase{core.PhaseDeploy, core.PhaseVerify} {
		if failed, err := r.phase(ctx, ph); err != nil || failed != "" {
			return r.evidence(ctx, failed, err)
		}
	}
	return "", nil
}

// resetLeftOver resets a shared target another task left changed, then marks it as this task's
// to reset, before any step of this task changes it.
func (r *envRun) resetLeftOver(ctx context.Context) error {
	if r.env.Ephemeral {
		return nil // its own, released by the task that made it
	}
	res, now := DirtyResource(r.lease.Resource), time.Now()
	dirty, err := r.p.Environments.GetLease(ctx, res)
	switch {
	case core.IsKind(err, core.KindNotFound):
	case err != nil:
		return err
	case dirty.Holder != r.jc.Task.ID:
		failed, err := r.phase(ctx, core.PhaseRelease)
		if err != nil {
			return err // the marker stays, so the reset is tried again
		}
		if failed != "" {
			return r.quarantine(ctx, fmt.Sprintf("resetting it after task %s: step %s failed",
				dirty.Holder, failed))
		}
		if err := r.p.Environments.ReleaseLease(ctx, res, dirty.Holder); err != nil {
			return err
		}
	}
	_, err = r.p.Environments.AcquireLease(ctx, core.Lease{Resource: res, Holder: r.jc.Task.ID,
		AcquiredAt: now, ExpiresAt: forever}, now)
	return err
}

// evidence gathers diagnostics after a failure and adds them to the failed step's output, which
// is what goes back to the implementer.
func (r *envRun) evidence(ctx context.Context, failed string, err error) (string, error) {
	if err != nil {
		return "", err
	}
	before := len(r.checks)
	if _, err := r.phase(ctx, core.PhaseEvidence); err != nil {
		return "", err
	}
	if before == 0 {
		return failed, nil
	}
	var b strings.Builder
	for _, c := range r.checks[before:] {
		fmt.Fprintf(&b, "\n--- evidence: %s ---\n%s", c.Name, c.Output)
	}
	r.checks[before-1].Output += b.String()
	return failed, nil
}

// phase runs a phase's steps and returns the first that failed. Evidence steps all run.
func (r *envRun) phase(ctx context.Context, ph core.EnvironmentPhase) (string, error) {
	failed := ""
	for _, s := range r.pr.Environment.Steps() {
		if s.Phase != ph {
			continue
		}
		ok, err := r.step(ctx, s)
		if err != nil {
			return "", err
		}
		if !ok && failed == "" {
			failed = s.Name
			if ph != core.PhaseEvidence {
				return failed, nil
			}
		}
	}
	return failed, nil
}

// step runs one step, recording a step that is not idempotent while it runs, and keeps what
// it did as evidence and in the audit log.
func (r *envRun) step(ctx context.Context, s core.EnvironmentStep) (bool, error) {
	if !s.Idempotent && s.Kind != core.OperationRead {
		if err := r.note(ctx, noteRunning, s.Name); err != nil {
			return false, err
		}
	}
	r.n++
	res, err := r.pr.Environment.RunStep(ctx, core.StepRequest{Env: r.env, Step: s,
		Holder: r.jc.Task.ID, SHA: r.sha, Runtime: r.pr.Implementer.Runtime, Workspace: r.ws,
		WorkDir: workspace.ScratchMount + "/repo", Labels: r.p.labels(r.jc),
		WorkloadName: workloadName(r.jc, fmt.Sprintf("env-%d", r.n))})
	if err != nil {
		return false, err
	}
	if err := r.note(ctx, noteRunning, ""); err != nil {
		return false, err
	}
	status := core.ValidationPassed
	if !res.OK {
		status = core.ValidationFailed
	}
	r.checks = append(r.checks, core.ValidationCheck{
		Name:  fmt.Sprintf("%s on %s/%s", s.Name, r.env.Provider, r.env.Target),
		Stage: "environment:" + string(s.Phase), Status: status, ExitCode: res.ExitCode,
		Output: res.Output, Optional: s.Phase == core.PhaseEvidence})
	r.jc.Log("environment", fmt.Sprintf("%s step %s on %s at %s: %s (exit %d)\n%s", s.Kind, s.Name,
		r.env.Target, r.sha, status, res.ExitCode, res.Output))
	r.p.Log.Info("environment operation", "task", r.jc.Task.ID, "environment", r.env.Provider,
		"target", r.env.Target, "step", s.Name, "kind", s.Kind, "sha", r.sha, "ok", res.OK)
	return res.OK, nil
}

// note records progress in the lease, and renews it.
func (r *envRun) note(ctx context.Context, key, value string) error {
	data := maps.Clone(r.lease.Data)
	if data == nil {
		data = map[string]string{}
	}
	if value == "" {
		delete(data, key)
	} else {
		data[key] = value
	}
	l := r.lease
	l.Data = data
	ttl := r.pr.EnvironmentLease
	if ttl <= 0 {
		ttl = defaultEnvironmentLease
	}
	if l.ExpiresAt.Before(forever) {
		l.ExpiresAt = time.Now().Add(ttl)
	}
	if err := r.p.Environments.UpdateLease(ctx, l); err != nil {
		return err
	}
	r.lease = l
	return nil
}

// release resets or destroys the environment and gives the lease up. When that fails, the
// lease is kept, marked broken, so no other task deploys onto a target in an unknown state. A
// release cut short by a transient error is tried again instead, unless the step it cut short
// cannot safely run twice.
func (r *envRun) release(ctx context.Context) error {
	if step := r.lease.Data[noteRunning]; step != "" {
		return r.quarantine(ctx, "step "+step+" was interrupted and cannot safely run twice")
	}
	failed, err := r.phase(ctx, core.PhaseRelease)
	if core.IsKind(err, core.KindTransient) || core.IsKind(err, core.KindCancelled) {
		return err // nothing is known to have gone wrong: the release is tried again
	}
	if err == nil && failed == "" {
		if err := r.p.Environments.ReleaseLease(ctx, DirtyResource(r.lease.Resource),
			r.jc.Task.ID); err != nil {
			return err
		}
		return r.p.Environments.ReleaseLease(ctx, r.lease.Resource, r.jc.Task.ID)
	}
	why := "step " + failed + " failed"
	if err != nil {
		why = core.UserMessage(err)
	}
	return r.quarantine(ctx, why)
}

// quarantine keeps the lease, marked broken, so no other task deploys onto a target in an
// unknown state, and says a person must restore it.
func (r *envRun) quarantine(ctx context.Context, why string) error {
	l := r.lease
	l.ExpiresAt = forever
	l.Data = maps.Clone(l.Data)
	if l.Data == nil {
		l.Data = map[string]string{}
	}
	l.Data[noteBroken] = why
	if uerr := r.p.Environments.UpdateLease(ctx, l); uerr != nil {
		return uerr
	}
	r.p.Log.Warn("environment release failed: a person must restore it", "target", r.env.Target,
		"task", r.jc.Task.ID, "reason", why)
	return core.Errorf(core.KindForbidden, "pipeline.environment",
		"releasing %s failed (%s); it stays reserved until a person restores it", r.env.Target, why)
}

// releaseEnvironments releases what a finished task still holds, for its cleanup job.
func (p *Pipeline) releaseEnvironments(ctx context.Context, jc scheduler.JobContext,
	pr *Project) error {
	if pr.Environment == nil || p.Environments == nil {
		return nil
	}
	name := pr.Environment.Info().Name
	leases, err := p.Environments.ListLeases(ctx, leaseResource(name, ""))
	if err != nil {
		return err
	}
	for _, l := range leases {
		if l.Holder != jc.Task.ID || l.Data[noteBroken] != "" {
			continue
		}
		target := strings.TrimPrefix(l.Resource, leaseResource(name, ""))
		r := &envRun{p: p, jc: jc, pr: pr, lease: l, sha: jc.Task.HeadSHA,
			ws: core.Mount{Volume: p.volume(jc.Task, "scratch"), Target: workspace.ScratchMount},
			env: core.Environment{Provider: name, Target: target,
				Ephemeral: len(pr.Environment.Targets()) == 0}}
		if err := r.release(ctx); err != nil {
			return err
		}
	}
	return nil
}
