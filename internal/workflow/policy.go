package workflow

import (
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

// Policy holds per-project workflow rules.
type Policy struct {
	// ApproveCommand is the comment command approving a design revision.
	ApproveCommand string
	// MinApprovePermission is the least access needed to approve or to operate.
	MinApprovePermission core.Permission
	// MinInputPermission is the least access for comments to reach the planner.
	MinInputPermission core.Permission
	// IntakeLabel, when set, makes an item enter the workflow only when a permitted
	// actor applies this label. Otherwise a permitted author opening one suffices.
	IntakeLabel string
	// ReviewPullRequests starts review-only tasks for pull requests by humans.
	ReviewPullRequests bool
	// BotLogins are identities that must never count as human approval.
	BotLogins []string

	MaxPlanLoops      int
	MaxImplementLoops int
	// MaxReviewFixLoops bounds how often a review can send the implementer back.
	MaxReviewFixLoops int
	// ReviewMinSeverity is the least severity of a finding worth a correction round. Those
	// below it, and those the reviewer has low confidence in, are published suggestions.
	ReviewMinSeverity core.ReviewSeverity
	QuotaPause        time.Duration
	AuthPause         time.Duration
	// StopAtReport ends a task at its report, leaving a pull request to people entirely.
	StopAtReport bool
	// TrustedReviewBots are automated reviewers whose remarks count as review input.
	TrustedReviewBots []string
	Merge             MergePolicy
	// SkipReviewTriage sends every remark straight to the implementer, without judging it.
	SkipReviewTriage bool
	// MaxDebateRounds bounds questions and objections per finding; MaxTriageRounds the
	// rounds of judging per task.
	MaxDebateRounds int
	MaxTriageRounds int
	// ReplyTimeout is how long an unanswered question stays quiet before a person is told.
	ReplyTimeout time.Duration
	// AgentLimitKeys groups agent jobs per account, for concurrency limits.
	PlannerLimitKey     string
	ImplementerLimitKey string
	ReviewerLimitKey    string
}

// DefaultPolicy returns conservative defaults.
func DefaultPolicy() Policy {
	return Policy{
		ApproveCommand:       "/approve-design",
		MinApprovePermission: core.PermissionWrite,
		MinInputPermission:   core.PermissionWrite,
		MaxPlanLoops:         8,
		MaxImplementLoops:    3,
		MaxReviewFixLoops:    3,
		ReviewMinSeverity:    core.ReviewMedium,
		Merge:                DefaultMergePolicy(),
		MaxDebateRounds:      2,
		MaxTriageRounds:      12,
		ReplyTimeout:         7 * 24 * time.Hour,
		QuotaPause:           30 * time.Minute,
		AuthPause:            time.Hour,
	}
}
