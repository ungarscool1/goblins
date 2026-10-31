// Package pipeline implements the job handlers: it connects workflow jobs to trackers,
// agents, runtimes and source control through contracts only.
//
// Handlers report results; only the workflow decides what they mean. Handlers must
// tolerate being run again, since an attempt can be interrupted at any point.
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	rt "github.com/ungarscool1/goblins/internal/runtime"
	"github.com/ungarscool1/goblins/internal/runtime/workspace"
	"github.com/ungarscool1/goblins/internal/scheduler"
	"github.com/ungarscool1/goblins/internal/workflow"
)

const (
	defaultGitImage = "goblins-git:dev"
	planBranch      = "goblins-plan"
)

// Stage is the resolved agent configuration for one role.
type Stage struct {
	Agent   core.AgentProvider
	Runtime core.ExecutionRuntime
	Model   string
	Effort  string
	Timeout time.Duration
}

// Project holds a project's resolved providers.
type Project struct {
	ID           string
	Repo         core.RepoRef
	BaseBranch   string
	Tracker      core.IssueTracker
	SCM          core.SourceControl
	Planner      Stage
	Implementer  Stage
	Validation   core.ValidationProvider
	Review       core.ReviewProvider
	Notification core.NotificationProvider
	GitImage     string
	Identity     workspace.Identity
	WebResearch  bool
	// MinInput is the access a commenter needs for their words to reach the planner.
	MinInput core.Permission
}

// Pipeline resolves projects and exposes the handlers.
type Pipeline struct {
	Installation string
	Projects     map[string]*Project
	Designs      interface {
		GetDesign(ctx context.Context, taskID string, revision int) (core.Design, error)
	}
	Log *slog.Logger
}

// Handlers returns the handler per job kind.
func (p *Pipeline) Handlers() map[core.JobKind]scheduler.Handler {
	return map[core.JobKind]scheduler.Handler{
		workflow.JobPlan:      p.plan,
		workflow.JobImplement: p.implement,
		workflow.JobValidate:  p.validate,
		workflow.JobPublish:   p.publish,
		workflow.JobReview:    p.review,
		workflow.JobComment:   p.comment,
		workflow.JobNotify:    p.notify,
		workflow.JobCleanup:   p.cleanup,
	}
}

func (p *Pipeline) project(id string) (*Project, error) {
	if pr, ok := p.Projects[id]; ok {
		return pr, nil
	}
	return nil, core.Errorf(core.KindInvalid, "pipeline", "project %s is no longer configured", id)
}

func issueRef(t core.Task) core.IssueRef {
	return core.IssueRef{Container: t.Subject.Container, ExternalID: t.Subject.ExternalID}
}

func decodePayload[T any](job core.Job) (T, error) {
	var v T
	if len(job.Payload) == 0 {
		return v, nil
	}
	if err := json.Unmarshal(job.Payload, &v); err != nil {
		return v, core.Wrap(core.KindInternal, "pipeline.payload", err)
	}
	return v, nil
}

// checkpoint is what an attempt leaves behind for the next one.
type checkpoint struct {
	SessionID string `json:"session_id,omitempty"`
}

func (p *Pipeline) labels(jc scheduler.JobContext) map[string]string {
	return map[string]string{
		scheduler.LabelInstallation: p.Installation, scheduler.LabelJob: jc.Job.ID,
		scheduler.LabelAttempt: strconv.Itoa(jc.Job.Attempt), scheduler.LabelTask: jc.Task.ID,
	}
}

// workloadName is unique per step and attempt, so that a retry never attaches to a
// workload left over from a dead attempt.
func workloadName(jc scheduler.JobContext, step string) string {
	return fmt.Sprintf("goblins-%s-%s-%d", jc.Job.ID, step, jc.Job.Attempt)
}

func gitImage(pr *Project) string {
	if pr.GitImage != "" {
		return pr.GitImage
	}
	return defaultGitImage
}

func (p *Pipeline) volume(task core.Task, purpose string) string {
	return workspace.VolumeName(p.Installation, task.ID, purpose)
}

// runAgent runs one agent round, resuming the most recent session and checkpointing a new
// one as soon as it is known, so a crash does not throw away paid-for work.
func (p *Pipeline) runAgent(ctx context.Context, jc scheduler.JobContext, stage Stage, role core.AgentRole, req core.AgentRequest) (core.AgentResult, error) {
	cp, _ := decodeCheckpoint(jc.Job.Checkpoint)
	session := cp.SessionID
	if session == "" {
		session = jc.Task.Sessions[role]
	}
	req.Role, req.Model, req.Effort, req.Runtime, req.Timeout = role, stage.Model, stage.Effort, stage.Runtime, stage.Timeout
	req.IdempotencyKey = jc.Job.IdempotencyKey
	req.WorkloadName = workloadName(jc, "agent")
	req.Labels = p.labels(jc)
	req.ResumeSessionID = session

	sink := func(e core.AgentEvent) {
		switch e.Kind {
		case core.AgentEventSession:
			if e.SessionID != cp.SessionID {
				cp.SessionID = e.SessionID
				if err := jc.SaveCheckpoint(ctx, cp); err != nil {
					p.Log.Warn("cannot checkpoint session", "job", jc.Job.ID, "err", err)
				}
			}
		case core.AgentEventText, core.AgentEventTool:
			jc.Log(string(e.Kind), e.Text)
		}
	}
	res, err := stage.Agent.Run(ctx, req, sink)
	if core.IsKind(err, core.KindNotFound) && session != "" {
		// The session is gone, for instance because its volume was lost. The durable
		// record (ticket, design) is the source of truth, so start over from it.
		jc.Log("system", "previous agent session is gone, starting a new one")
		req.ResumeSessionID = ""
		req.WorkloadName = workloadName(jc, "agent-fresh")
		cp.SessionID = ""
		return stage.Agent.Run(ctx, req, sink)
	}
	return res, err
}

func decodeCheckpoint(raw json.RawMessage) (checkpoint, error) {
	var cp checkpoint
	if len(raw) == 0 {
		return cp, nil
	}
	return cp, json.Unmarshal(raw, &cp)
}

// runStep runs a workspace operation and parses its result.
func runStep(ctx context.Context, runtime core.ExecutionRuntime, spec core.WorkloadSpec) (workspace.Result, rt.Result, error) {
	res, err := rt.RunToCompletion(ctx, runtime, spec)
	if err != nil {
		return workspace.Result{}, res, err
	}
	if res.Status.Phase != core.WorkloadSucceeded {
		return workspace.Result{}, res, core.Errorf(core.KindTransient, "pipeline.step", "workspace step %s failed (exit %d)", spec.Name, res.Status.ExitCode)
	}
	parsed, err := workspace.ParseResult(res.Stdout)
	return parsed, res, err
}

// prepare ensures a pinned checkout on a volume. With reuse, an existing checkout is kept,
// which is what lets later implementation rounds continue on earlier work.
func (p *Pipeline) prepare(ctx context.Context, jc scheduler.JobContext, pr *Project, runtime core.ExecutionRuntime, volume, branch string, reuse bool) (workspace.Result, error) {
	if err := runtime.EnsureVolume(ctx, volume, map[string]string{scheduler.LabelInstallation: p.Installation, scheduler.LabelTask: jc.Task.ID}); err != nil {
		return workspace.Result{}, err
	}
	image := gitImage(pr)
	if reuse {
		st, _, err := runStep(ctx, runtime, workspace.InspectSpec(workloadName(jc, "inspect"), image, volume, p.labels(jc)))
		if err != nil {
			return workspace.Result{}, err
		}
		if st.Prepared {
			return st, nil
		}
	}
	base := pr.BaseBranch
	sha, err := pr.SCM.ResolveBranch(ctx, pr.Repo, base)
	if err != nil {
		return workspace.Result{}, err
	}
	access, err := pr.SCM.GitAccess(ctx, pr.Repo, false)
	if err != nil {
		return workspace.Result{}, err
	}
	spec, err := workspace.PrepareSpec(workloadName(jc, "prepare"), image, volume, p.labels(jc), access, base, sha, branch)
	if err != nil {
		return workspace.Result{}, err
	}
	res, _, err := runStep(ctx, runtime, spec)
	return res, err
}

// visibleComments returns the conversation the planner may see: people with sufficient
// access, this workflow's own posts, and replies through the manual inbox, which were
// checked when they arrived. Others are withheld, so that neither a stranger's comment nor
// other automation can feed instructions to the model. Permission lookups fail closed.
func (p *Pipeline) visibleComments(ctx context.Context, pr *Project, task core.Task, ref core.IssueRef) ([]core.Comment, int, error) {
	all, err := pr.Tracker.ListComments(ctx, ref, time.Time{})
	if err != nil {
		return nil, 0, err
	}
	perms := map[string]core.Permission{}
	out := make([]core.Comment, 0, len(all))
	withheld := 0
	for _, c := range all {
		if c.Own {
			out = append(out, c)
			continue
		}
		if c.Author.Type != core.ActorHuman {
			withheld++
			continue
		}
		perm, known := perms[c.Author.Login]
		if !known {
			perm, err = pr.Tracker.ActorPermission(ctx, ref, c.Author)
			if err != nil {
				if core.IsKind(err, core.KindTransient) || core.IsKind(err, core.KindCancelled) {
					return nil, 0, err
				}
				perm = core.PermissionNone
			}
			perms[c.Author.Login] = perm
		}
		if perm >= pr.MinInput {
			out = append(out, c)
		} else {
			withheld++
		}
	}
	out = append(out, task.ManualInput...)
	slices.SortStableFunc(out, func(a, b core.Comment) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out, withheld, nil
}

func (p *Pipeline) plan(ctx context.Context, jc scheduler.JobContext) (any, error) {
	pr, err := p.project(jc.Task.Project)
	if err != nil {
		return nil, err
	}
	payload, err := decodePayload[workflow.PlanPayload](jc.Job)
	if err != nil {
		return nil, err
	}
	ref := issueRef(jc.Task)
	issue, err := pr.Tracker.GetIssue(ctx, ref)
	if err != nil {
		return nil, err
	}
	comments, withheld, err := p.visibleComments(ctx, pr, jc.Task, ref)
	if err != nil {
		return nil, err
	}
	volume := p.volume(jc.Task, "plan")
	if _, err := p.prepare(ctx, jc, pr, pr.Planner.Runtime, volume, planBranch, false); err != nil {
		return nil, err
	}
	res, err := p.runAgent(ctx, jc, pr.Planner, core.RolePlanner, core.AgentRequest{
		SystemPrompt: plannerSystem,
		Prompt:       PlannerPrompt(issue, comments, withheld, payload.Trigger),
		Policy:       core.ToolPolicy{Web: pr.WebResearch},
		Workspace:    core.Mount{Volume: volume, Target: workspace.Mount, ReadOnly: true},
		WorkDir:      workspace.RepoDir,
	})
	if err != nil {
		return nil, err
	}
	questions, design, perr := ParsePlan(res.Output)
	if perr != nil {
		jc.Log("system", "planner output rejected: "+perr.Error())
		if jc.Job.Attempt < 2 {
			return nil, core.Errorf(core.KindTransient, "pipeline.plan", "the planner returned an unusable result")
		}
		return nil, core.Errorf(core.KindInvalid, "pipeline.plan", "the planner repeatedly returned an unusable result")
	}
	return workflow.PlanResult{SessionID: res.SessionID, Questions: questions, Design: design}, nil
}

// approvedDesign rechecks the approval right where it matters, independently of the state
// machine that scheduled this job. Implementation never runs on anything else.
func (p *Pipeline) approvedDesign(ctx context.Context, task core.Task, revision int) (core.Design, error) {
	a := task.Approval
	if a == nil || a.Revision != revision || a.Revision != task.DesignRevision {
		return core.Design{}, core.Errorf(core.KindForbidden, "pipeline.approval", "no valid approval exists for design revision %d", revision)
	}
	d, err := p.Designs.GetDesign(ctx, task.ID, revision)
	if err != nil {
		return core.Design{}, err
	}
	if d.Hash != a.Hash || core.Digest(d.Body) != a.Hash {
		return core.Design{}, core.Errorf(core.KindForbidden, "pipeline.approval", "the stored design does not match its approval")
	}
	return d, nil
}

func (p *Pipeline) implement(ctx context.Context, jc scheduler.JobContext) (any, error) {
	pr, err := p.project(jc.Task.Project)
	if err != nil {
		return nil, err
	}
	payload, err := decodePayload[workflow.ImplementPayload](jc.Job)
	if err != nil {
		return nil, err
	}
	design, err := p.approvedDesign(ctx, jc.Task, payload.Revision)
	if err != nil {
		return nil, err
	}
	issue, err := pr.Tracker.GetIssue(ctx, issueRef(jc.Task))
	if err != nil {
		return nil, err
	}
	branch := workspace.BranchName(jc.Task.ID)
	volume := p.volume(jc.Task, "impl")
	runtime := pr.Implementer.Runtime
	if _, err := p.prepare(ctx, jc, pr, runtime, volume, branch, true); err != nil {
		return nil, err
	}
	res, err := p.runAgent(ctx, jc, pr.Implementer, core.RoleImplementer, core.AgentRequest{
		SystemPrompt: implementerSystem,
		Prompt:       ImplementerPrompt(issue, design, payload.Feedback),
		Policy:       core.ToolPolicy{WriteFiles: true},
		Workspace:    core.Mount{Volume: volume, Target: workspace.Mount},
		WorkDir:      workspace.RepoDir,
	})
	if err != nil {
		return nil, err
	}
	message := fmt.Sprintf("%s\n\nImplements design revision %d, approved by %s.", jc.Task.Title, design.Revision, jc.Task.Approval.Approver.Login)
	spec, err := workspace.CommitSpec(workloadName(jc, "commit"), gitImage(pr), volume, p.labels(jc), pr.Identity, branch, message)
	if err != nil {
		return nil, err
	}
	st, _, err := runStep(ctx, runtime, spec)
	if err != nil {
		return nil, err
	}
	return workflow.ImplementResult{SessionID: res.SessionID, Workspace: volume, Branch: branch, BaseSHA: st.BaseSHA, HeadSHA: st.HeadSHA, Changed: st.Changed}, nil
}

func (p *Pipeline) validate(ctx context.Context, jc scheduler.JobContext) (any, error) {
	pr, err := p.project(jc.Task.Project)
	if err != nil {
		return nil, err
	}
	payload, err := decodePayload[workflow.ValidatePayload](jc.Job)
	if err != nil {
		return nil, err
	}
	if payload.Reuse {
		return workflow.ValidateResult{Report: core.ValidationReport{Status: core.ValidationPassed, Summary: "this exact commit already passed before publishing"}, Skipped: pr.Validation == nil}, nil
	}
	if pr.Validation == nil {
		return workflow.ValidateResult{Report: core.ValidationReport{Status: core.ValidationPassed, Summary: "no validation is configured for this project"}, Skipped: true}, nil
	}
	runtime := pr.Implementer.Runtime
	scratch := p.volume(jc.Task, "scratch")
	if err := runtime.EnsureVolume(ctx, scratch, map[string]string{scheduler.LabelInstallation: p.Installation, scheduler.LabelTask: jc.Task.ID}); err != nil {
		return nil, err
	}
	spec, err := workspace.CopySpec(workloadName(jc, "copy"), gitImage(pr), p.volume(jc.Task, "impl"), scratch, p.labels(jc), payload.SHA)
	if err != nil {
		return nil, err
	}
	if _, _, err := runStep(ctx, runtime, spec); err != nil {
		return nil, err
	}
	report, err := pr.Validation.Validate(ctx, core.ValidationRequest{
		IdempotencyKey: jc.Job.IdempotencyKey, WorkloadName: workloadName(jc, "check"), HeadSHA: payload.SHA,
		Workspace: core.Mount{Volume: scratch, Target: workspace.ScratchMount},
		WorkDir:   workspace.ScratchMount + "/repo",
		Runtime:   runtime, Labels: p.labels(jc),
	})
	if err != nil {
		return nil, err
	}
	for _, c := range report.Checks {
		jc.Log("validation", fmt.Sprintf("%s: %s (exit %d)\n%s", c.Name, c.Status, c.ExitCode, c.Output))
	}
	return workflow.ValidateResult{Report: report}, nil
}

func (p *Pipeline) publish(ctx context.Context, jc scheduler.JobContext) (any, error) {
	pr, err := p.project(jc.Task.Project)
	if err != nil {
		return nil, err
	}
	payload, err := decodePayload[workflow.PublishPayload](jc.Job)
	if err != nil {
		return nil, err
	}
	if _, err := p.approvedDesign(ctx, jc.Task, payload.Revision); err != nil {
		return nil, err
	}
	if payload.SHA != jc.Task.ValidatedSHA {
		return nil, core.Errorf(core.KindForbidden, "pipeline.publish", "commit %s was not validated", payload.SHA)
	}
	access, err := pr.SCM.GitAccess(ctx, pr.Repo, true)
	if err != nil {
		return nil, err
	}
	branch := workspace.BranchName(jc.Task.ID)
	spec, err := workspace.PublishSpec(workloadName(jc, "publish"), gitImage(pr), p.volume(jc.Task, "impl"), p.labels(jc), access, pr.BaseBranch, branch, payload.SHA)
	if err != nil {
		return nil, err
	}
	res, err := rt.RunToCompletion(ctx, pr.Implementer.Runtime, spec)
	if err != nil {
		return nil, err
	}
	if res.Status.Phase != core.WorkloadSucceeded {
		return nil, workspace.ClassifyPush(res.Stderr)
	}
	issue, err := pr.Tracker.GetIssue(ctx, issueRef(jc.Task))
	if err != nil {
		return nil, err
	}
	pull, created, err := pr.SCM.EnsurePullRequest(ctx, core.PullRequestDraft{
		IdempotencyKey: jc.Task.ID + "/pr", Repo: pr.Repo, Head: branch, Base: pr.BaseBranch, Draft: true,
		Title: jc.Task.Title, Body: pullRequestBody(jc.Task, issue, payload),
	})
	if err != nil {
		return nil, err
	}
	return workflow.PublishResult{PullRequest: pull, Created: created}, nil
}

func pullRequestBody(task core.Task, issue core.Issue, payload workflow.PublishPayload) string {
	var b strings.Builder
	link := issue.URL
	if link == "" {
		link = issue.Ref.ExternalID
	}
	fmt.Fprintf(&b, "Goblins opened this draft for %s.\n\n", link)
	fmt.Fprintf(&b, "- Design: revision %d (`%s`), approved by @%s\n", payload.Revision, task.Approval.Hash[:12], task.Approval.Approver.Login)
	fmt.Fprintf(&b, "- Validated commit: `%s`\n\n", payload.SHA)
	b.WriteString("It was written by an automated agent and has not been reviewed by a person. Goblins does not merge pull requests.")
	return b.String()
}

func (p *Pipeline) review(ctx context.Context, jc scheduler.JobContext) (any, error) {
	pr, err := p.project(jc.Task.Project)
	if err != nil {
		return nil, err
	}
	if pr.Review == nil {
		return workflow.ReviewResult{Result: core.ReviewResult{Verdict: core.VerdictSkipped, Summary: "no independent reviewer is configured"}}, nil
	}
	if jc.Task.PullRequest == nil {
		return nil, core.Errorf(core.KindInvalid, "pipeline.review", "the task has no pull request to review")
	}
	design := ""
	if a := jc.Task.Approval; a != nil {
		if d, err := p.approvedDesign(ctx, jc.Task, a.Revision); err == nil {
			design = d.Body
		}
	}
	res, err := pr.Review.Review(ctx, core.ReviewRequest{IdempotencyKey: jc.Job.IdempotencyKey, Repo: pr.Repo, PullRequest: *jc.Task.PullRequest, Design: design})
	if err != nil {
		return nil, err
	}
	if res.Verdict != core.VerdictSkipped {
		err := pr.SCM.PostReview(ctx, pr.Repo, jc.Task.PullRequest.Number, core.ReviewDraft{IdempotencyKey: jc.Job.IdempotencyKey, Verdict: res.Verdict, Body: res.Summary, Comments: res.Findings})
		if err != nil {
			return nil, err
		}
	}
	return workflow.ReviewResult{Result: res}, nil
}

// comment posts to the conversation. If the tracker cannot take comments, the message goes
// to the operator through a notification, which is the manual inbox of P0. With neither,
// the failure surfaces, so the task does not silently wait for an answer nobody could give.
func (p *Pipeline) comment(ctx context.Context, jc scheduler.JobContext) (any, error) {
	pr, err := p.project(jc.Task.Project)
	if err != nil {
		return nil, err
	}
	payload, err := decodePayload[workflow.CommentPayload](jc.Job)
	if err != nil {
		return nil, err
	}
	draft := core.CommentDraft{IdempotencyKey: jc.Job.IdempotencyKey, Body: payload.Body}
	if jc.Task.Subject.Kind == core.SubjectPullRequest {
		err = pr.SCM.PostPullRequestComment(ctx, pr.Repo, jc.Task.Subject.ExternalID, draft)
	} else {
		_, err = pr.Tracker.PostComment(ctx, issueRef(jc.Task), draft)
	}
	if core.IsKind(err, core.KindUnsupported) && pr.Notification != nil {
		note := core.Notification{IdempotencyKey: jc.Job.IdempotencyKey, Project: pr.ID, Subject: jc.Task.Subject, Severity: core.SeverityAction,
			Title: "Message for " + jc.Task.Title, Body: payload.Body}
		if nerr := pr.Notification.Notify(ctx, note); nerr != nil {
			return nil, nerr
		}
		return map[string]string{"via": "notification"}, nil
	}
	if err != nil {
		return nil, err
	}
	return map[string]string{"via": "tracker"}, nil
}

func (p *Pipeline) notify(ctx context.Context, jc scheduler.JobContext) (any, error) {
	pr, err := p.project(jc.Task.Project)
	if err != nil {
		return nil, err
	}
	payload, err := decodePayload[workflow.NotifyPayload](jc.Job)
	if err != nil {
		return nil, err
	}
	if pr.Notification == nil {
		p.Log.Info("operator notification", "project", pr.ID, "task", jc.Task.ID, "severity", payload.Severity, "title", payload.Title, "body", payload.Body)
		return struct{}{}, nil
	}
	return struct{}{}, pr.Notification.Notify(ctx, core.Notification{IdempotencyKey: jc.Job.IdempotencyKey, Project: pr.ID, Subject: jc.Task.Subject, Severity: payload.Severity, Title: payload.Title, Body: payload.Body})
}

func (p *Pipeline) cleanup(ctx context.Context, jc scheduler.JobContext) (any, error) {
	pr, err := p.project(jc.Task.Project)
	if err != nil {
		return nil, err
	}
	var errs []error
	for _, v := range []struct {
		runtime core.ExecutionRuntime
		purpose string
	}{{pr.Planner.Runtime, "plan"}, {pr.Implementer.Runtime, "impl"}, {pr.Implementer.Runtime, "scratch"}} {
		if err := v.runtime.RemoveVolume(ctx, p.volume(jc.Task, v.purpose)); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, core.Wrap(core.KindTransient, "pipeline.cleanup", err)
	}
	return struct{}{}, nil
}
