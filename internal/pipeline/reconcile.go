package pipeline

import (
	"context"
	"slices"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/runtime/workspace"
	"github.com/ungarscool1/goblins/internal/scheduler"
	"github.com/ungarscool1/goblins/internal/workflow"
)

// reconcile reads where a pull request stands now, from the SCM, whatever led to asking. It
// changes nothing and uses no model. Permission lookups fail closed, and remarks by anyone
// without access are withheld here, so they reach neither the workflow nor a prompt.
func (p *Pipeline) reconcile(ctx context.Context, jc scheduler.JobContext) (any, error) {
	pr, err := p.project(jc.Task.Project)
	if err != nil {
		return nil, err
	}
	if jc.Task.PullRequest == nil {
		return nil, core.Errorf(core.KindInvalid, "pipeline.reconcile",
			"the task has no pull request")
	}
	number := jc.Task.PullRequest.Number
	pull, err := pr.SCM.GetPullRequest(ctx, pr.Repo, number)
	if err != nil {
		return nil, err
	}
	res := workflow.ReconcileResult{PullRequest: pull,
		CanMerge: pr.SCM.Info().Has(core.CapSCMMerge)}
	if !pull.Open {
		return res, nil
	}
	perms := permissionCache{p: pr, ref: issueRef(jc.Task), known: map[string]core.Permission{}}
	if res.Reviews, err = p.reviewsWithPermission(ctx, pr, number, &perms); err != nil {
		return nil, err
	}
	threads, err := pr.SCM.ReviewThreads(ctx, pr.Repo, number)
	if err != nil {
		return nil, err
	}
	head := pull.HeadSHA
	if res.Findings, err = threadFindings(ctx, threads, head, &perms); err != nil {
		return nil, err
	}
	reviewFs, err := reviewFindings(ctx, res.Reviews, head, &perms)
	if err != nil {
		return nil, err
	}
	res.Findings = append(res.Findings, reviewFs...)
	if pr.SCM.Info().Has(core.CapSCMStatusChecks) {
		if res.Checks, err = pr.SCM.StatusChecks(ctx, pr.Repo, head); err != nil {
			return nil, err
		}
	}
	if res.Files, err = pr.SCM.ChangedFiles(ctx, pr.Repo, number); err != nil {
		return nil, err
	}
	res.Protection = p.protection(ctx, jc, pr, pull.Base)
	return res, nil
}

// protection reports what the SCM enforces, or nothing if it cannot say: unknown is not the
// same as unprotected, and the gate treats it as not known to be protected.
func (p *Pipeline) protection(ctx context.Context, jc scheduler.JobContext, pr *Project,
	base string) *core.BranchProtection {
	if !pr.SCM.Info().Has(core.CapSCMBranchProtection) {
		return nil
	}
	bp, err := pr.SCM.BranchProtection(ctx, pr.Repo, base)
	if err != nil {
		jc.Log("system", "branch protection is unknown: "+core.UserMessage(err))
		return nil
	}
	return &bp
}

type permissionCache struct {
	p     *Project
	ref   core.IssueRef
	known map[string]core.Permission
}

// of looks up access, treating a permanent failure as none. A transient one is returned, as
// not knowing is different from being refused.
func (c *permissionCache) of(ctx context.Context, a core.Actor) (core.Permission, error) {
	if perm, ok := c.known[a.Login]; ok {
		return perm, nil
	}
	perm, err := c.p.Tracker.ActorPermission(ctx, c.ref, a)
	if core.IsKind(err, core.KindTransient) || core.IsKind(err, core.KindCancelled) {
		return core.PermissionNone, err
	}
	if err != nil {
		perm = core.PermissionNone
	}
	c.known[a.Login] = perm
	return perm, nil
}

// admits says whether someone's remarks count as review input.
func (c *permissionCache) admits(ctx context.Context, a core.Actor) (bool, error) {
	if a.Type != core.ActorHuman {
		return slices.Contains(c.p.TrustedReviewBots, a.Login), nil
	}
	perm, err := c.of(ctx, a)
	return perm >= c.p.MinInput, err
}

func (p *Pipeline) reviewsWithPermission(ctx context.Context, pr *Project, number string,
	perms *permissionCache) ([]core.Review, error) {
	reviews, err := pr.SCM.ListReviews(ctx, pr.Repo, number)
	if err != nil {
		return nil, err
	}
	for i := range reviews {
		if reviews[i].Author.Type != core.ActorHuman {
			continue
		}
		if reviews[i].Permission, err = perms.of(ctx, reviews[i].Author); err != nil {
			return nil, err
		}
	}
	return reviews, nil
}

// threadFindings normalizes review remarks. A remark about lines the head no longer has is
// stale, and says so instead of disappearing.
func threadFindings(ctx context.Context, threads []core.ReviewThread, head string,
	perms *permissionCache) ([]core.ReviewFinding, error) {
	var out []core.ReviewFinding
	for _, t := range threads {
		ok, err := perms.admits(ctx, t.Author)
		if err != nil {
			return nil, err
		}
		if t.Own || !ok {
			continue
		}
		f := core.ReviewFinding{ID: "thread:" + t.ID, ThreadID: t.ThreadID, Author: t.Author,
			Path: t.Path, Line: t.Line,
			Severity: core.ReviewHigh, Confidence: core.ConfidenceHigh, Body: capText(t.Body,
				maxFindingBytes),
			HeadSHA: t.CommitSHA, Provenance: t.Author.Login, Status: core.FindingOpen}
		switch {
		case t.Resolved:
			f.Status = core.FindingResolved
		case t.Outdated && t.CommitSHA != head:
			f.Status = core.FindingStale
		}
		out = append(out, f)
	}
	return out, nil
}

// reviewFindings turns the text of a change request into a finding. A review on an earlier
// commit is stale as a finding, while the verdict itself keeps standing until cleared.
func reviewFindings(ctx context.Context, reviews []core.Review, head string,
	perms *permissionCache) ([]core.ReviewFinding, error) {
	var out []core.ReviewFinding
	for _, r := range reviews {
		ok, err := perms.admits(ctx, r.Author)
		if err != nil {
			return nil, err
		}
		if r.Own || !ok || r.Verdict != core.VerdictChangesRequested || r.Body == "" {
			continue
		}
		f := core.ReviewFinding{ID: "review:" + r.ID, Author: r.Author, Severity: core.ReviewHigh,
			Confidence: core.ConfidenceHigh,
			Body:       capText(r.Body, maxFindingBytes), HeadSHA: r.CommitSHA,
			Provenance: r.Author.Login, Status: core.FindingOpen}
		if r.CommitSHA != head || r.Dismissed {
			f.Status = core.FindingStale
		}
		out = append(out, f)
	}
	return out, nil
}

// merge merges one exact commit under an expressly approved rule. It rechecks what it can
// right where it matters; the SCM refuses if the head has moved.
func (p *Pipeline) merge(ctx context.Context, jc scheduler.JobContext) (any, error) {
	pr, err := p.project(jc.Task.Project)
	if err != nil {
		return nil, err
	}
	payload, err := decodePayload[workflow.MergePayload](jc.Job)
	if err != nil {
		return nil, err
	}
	if payload.Rule == "" || jc.Task.Gate == nil || jc.Task.Gate.State != core.GateAutoMerge || jc.Task.Gate.SHA != payload.SHA {
		return nil, core.Errorf(core.KindForbidden, "pipeline.merge",
			"no merge rule allows merging commit %s", payload.SHA)
	}
	if err := core.Negotiate(pr.SCM, core.CapSCMMerge); err != nil {
		return nil, err
	}
	return struct{}{}, pr.SCM.MergePullRequest(ctx, pr.Repo, jc.Task.PullRequest.Number,
		payload.SHA)
}

// syncWorkspace brings the implementer's workspace to the commit the pull request is at, when
// that is not what it holds: someone else pushed, or the volume was lost. Like any checkout, it
// starts from empty, so a token never meets anything untrusted code could have touched.
func (p *Pipeline) syncWorkspace(ctx context.Context, jc scheduler.JobContext, pr *Project,
	sha string) error {
	if jc.Task.PullRequest == nil || sha == "" {
		return nil
	}
	runtime, image, volume := pr.Implementer.Runtime, gitImage(pr), p.volume(jc.Task, "impl")
	st, _, err := runStep(ctx, runtime, workspace.InspectSpec(workloadName(jc, "sync-inspect"),
		image, volume, p.labels(jc)))
	if err != nil || st.HeadSHA == sha {
		return err
	}
	access, err := pr.SCM.GitAccess(ctx, pr.Repo, false)
	if err != nil {
		return err
	}
	branch := workspace.BranchName(jc.Task.ID)
	spec, err := workspace.PrepareSpec(workloadName(jc, "sync"), image, volume, p.labels(jc),
		access, branch, sha, branch)
	if err != nil {
		return err
	}
	_, _, err = runStep(ctx, runtime, spec)
	return err
}
