package app

import (
	"slices"

	"github.com/ungarscool1/goblins/internal/config"
	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/workflow"
)

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
	if c.RateLimitPause > 0 {
		p.RateLimitPause = c.RateLimitPause
	}
	if c.MaxLimitPause > 0 {
		p.MaxLimitPause = c.MaxLimitPause
	}
	if c.MaxLimitPauses > 0 {
		p.MaxLimitPauses = c.MaxLimitPauses
	}
	p.MaxChangedFiles = c.MaxChangedFiles
	p.MaxTaskTokens = c.MaxTaskTokens
	if c.MaxReviewFixLoops > 0 {
		p.MaxReviewFixLoops = c.MaxReviewFixLoops
	}
	if c.ReviewMinSeverity != "" {
		p.ReviewMinSeverity = core.ReviewSeverity(c.ReviewMinSeverity)
	}
	p.StopAtReport = c.StopAtReport
	p.TrustedReviewBots = c.TrustedReviewBots
	p.Merge = mergePolicy(c.Merge)
	p.Approval = workflow.ApprovalProfile{
		Unattended: c.Approval.Profile == config.ApprovalUnattended,
		Label:      c.Approval.Label, AllowedPaths: c.Approval.AllowedPaths,
		DeniedPaths: slices.Concat(config.SensitivePaths, c.Approval.DeniedPaths,
			c.TrustedPaths),
		MaxChangedFiles: c.Approval.MaxChangedFiles}
	if c.ValidationManifest != "" {
		p.Approval.DeniedPaths = append(p.Approval.DeniedPaths, c.ValidationManifest)
	}
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
