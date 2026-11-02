package core

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"time"
)

// ActorType distinguishes people from automation. Only people can approve.
type ActorType string

const (
	ActorHuman   ActorType = "human"
	ActorBot     ActorType = "bot"
	ActorUnknown ActorType = "unknown"
)

// Actor is a canonical user identity on a provider.
type Actor struct {
	ID    string    `json:"id"`
	Login string    `json:"login"`
	Type  ActorType `json:"type"`
}

// Permission is a canonical, ordered access level.
type Permission int

const (
	PermissionNone Permission = iota
	PermissionRead
	PermissionTriage
	PermissionWrite
	PermissionAdmin
)

func (p Permission) String() string {
	return [...]string{"none", "read", "triage", "write", "admin"}[p]
}

// ParsePermission converts a permission name, defaulting to none.
func ParsePermission(s string) Permission {
	for _, p := range []Permission{PermissionRead, PermissionTriage, PermissionWrite, PermissionAdmin} {
		if p.String() == s {
			return p
		}
	}
	return PermissionNone
}

// SubjectKind says what a task or event is about.
type SubjectKind string

const (
	SubjectIssue       SubjectKind = "issue"
	SubjectPullRequest SubjectKind = "pull_request"
)

// Subject is a stable identifier of a tracked item across its lifetime.
type Subject struct {
	Kind       SubjectKind `json:"kind"`
	Provider   string      `json:"provider"` // configured instance name
	Container  string      `json:"container,omitempty"`
	ExternalID string      `json:"external_id"`
}

// Key returns the canonical string form used for locks and uniqueness.
func (s Subject) Key() string {
	return strings.Join([]string{string(s.Kind), s.Provider, s.Container, s.ExternalID}, "/")
}

// IssueRef identifies an issue on a tracker.
type IssueRef struct {
	Container  string `json:"container,omitempty"` // e.g. a repository or project key
	ExternalID string `json:"external_id"`
}

// Issue is the canonical form of a work item.
type Issue struct {
	Ref       IssueRef  `json:"ref"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Open      bool      `json:"open"`
	Labels    []string  `json:"labels,omitempty"`
	Author    Actor     `json:"author"`
	URL       string    `json:"url,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// RequirementsHash fingerprints the material content of the issue, so edits to
// labels or timestamps do not invalidate an approved design.
func (i Issue) RequirementsHash() string {
	return Digest(i.Title, i.Body)
}

// Digest returns the hex SHA-256 of length-delimited parts.
func Digest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_ = binary.Write(h, binary.BigEndian, uint64(len(p)))
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Comment is a conversation entry on an issue.
type Comment struct {
	ID        string    `json:"id"`
	Issue     IssueRef  `json:"issue"`
	Author    Actor     `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Own is set by trackers listing comments, on those this integration itself posted.
	// Being written by some bot does not imply it: other automation can relay anything.
	Own bool `json:"own,omitempty"`
}

// CommentDraft is a comment to publish. Trackers must treat IdempotencyKey as
// a promise: posting twice with one key yields one visible comment.
type CommentDraft struct {
	IdempotencyKey string
	Body           string
}

// IssueQuery filters ListIssues.
type IssueQuery struct {
	Container    string
	UpdatedSince time.Time
	OpenOnly     bool
	Limit        int
}

// RepoRef identifies a repository on an SCM.
type RepoRef struct {
	Slug string `json:"slug"`
}

// GitAccess is what a workload needs to reach a repository over Git. Short
// lived where the SCM allows it.
type GitAccess struct {
	RemoteURL string
	Username  string
	Password  Secret
	ExpiresAt time.Time
}

// Commit is a canonical commit.
type Commit struct {
	SHA     string `json:"sha"`
	Message string `json:"message"`
	Author  string `json:"author,omitempty"`
}

// PullRequestDraft describes a PR/MR to open or update.
type PullRequestDraft struct {
	IdempotencyKey string
	Repo           RepoRef
	Head           string
	Base           string
	Title          string
	Body           string
	Draft          bool
}

// PullRequest is the canonical form of a PR/MR.
type PullRequest struct {
	Repo    RepoRef `json:"repo"`
	Number  string  `json:"number"`
	URL     string  `json:"url,omitempty"`
	Title   string  `json:"title,omitempty"`
	Head    string  `json:"head"`
	HeadSHA string  `json:"head_sha,omitempty"`
	Base    string  `json:"base"`
	Draft   bool    `json:"draft"`
	Open    bool    `json:"open"`
	Merged  bool    `json:"merged,omitempty"`
	Author  Actor   `json:"author"`
	// FetchRef is where Git finds the head, when it lives somewhere other than a branch
	// of this repository, as for a pull request from a fork.
	FetchRef string `json:"fetch_ref,omitempty"`
}

// ReviewVerdict is the outcome of a review.
type ReviewVerdict string

const (
	VerdictApprove          ReviewVerdict = "approve"
	VerdictChangesRequested ReviewVerdict = "changes_requested"
	VerdictComment          ReviewVerdict = "comment"
	VerdictSkipped          ReviewVerdict = "skipped"
)

// InlineComment anchors a review remark to a file line.
type InlineComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Body string `json:"body"`
}

// ReviewSeverity grades a review finding.
type ReviewSeverity string

const (
	ReviewCritical ReviewSeverity = "critical"
	ReviewHigh     ReviewSeverity = "high"
	ReviewMedium   ReviewSeverity = "medium"
	ReviewLow      ReviewSeverity = "low"
	ReviewInfo     ReviewSeverity = "info"
)

// Rank orders severities; an unknown one ranks lowest.
func (s ReviewSeverity) Rank() int {
	switch s {
	case ReviewCritical:
		return 4
	case ReviewHigh:
		return 3
	case ReviewMedium:
		return 2
	case ReviewLow:
		return 1
	}
	return 0
}

// ReviewConfidence is how sure a reviewer says it is.
type ReviewConfidence string

const (
	ConfidenceHigh   ReviewConfidence = "high"
	ConfidenceMedium ReviewConfidence = "medium"
	ConfidenceLow    ReviewConfidence = "low"
)

// ReviewFinding is one normalized remark of a reviewer on one exact commit.
type ReviewFinding struct {
	// ID is stable across observations of the same remark.
	ID         string           `json:"id,omitempty"`
	ThreadID   string           `json:"thread_id,omitempty"`
	Author     Actor            `json:"author,omitzero"`
	Status     FindingStatus    `json:"status,omitempty"`
	Path       string           `json:"path,omitempty"`
	Line       int              `json:"line,omitempty"`
	Severity   ReviewSeverity   `json:"severity"`
	Confidence ReviewConfidence `json:"confidence"`
	Body       string           `json:"body"`
	HeadSHA    string           `json:"head_sha,omitempty"`
	// Provenance names what raised it: the provider and its prompt version, or a person.
	Provenance string `json:"provenance,omitempty"`
	// Discussion is the conversation about it, oldest first and bounded, so that it survives
	// restarts and lost agent sessions.
	Discussion []DiscussionEntry `json:"discussion,omitempty"`
	// LastCommentID tells whether someone has said something new.
	LastCommentID string `json:"last_comment_id,omitempty"`
	// Plan and Evidence are the agent's latest judgement, as data.
	Plan     string `json:"plan,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	// FixSHA is the correction commit, once there is one.
	FixSHA string `json:"fix_sha,omitempty"`
	// DebateRounds counts questions and objections posted, which a policy bounds.
	DebateRounds int `json:"debate_rounds,omitempty"`
	// History is the audit trail of who decided what, on which commit.
	History []FindingEvent `json:"history,omitempty"`
}

// DiscussionEntry is one remark in a finding's conversation.
type DiscussionEntry struct {
	CommentID string `json:"comment_id"`
	Author    string `json:"author"`
	Own       bool   `json:"own,omitempty"`
	Human     bool   `json:"human,omitempty"`
	Body      string `json:"body"`
}

// FindingEvent records one step of a finding.
type FindingEvent struct {
	At     time.Time     `json:"at"`
	Status FindingStatus `json:"status"`
	SHA    string        `json:"sha,omitempty"`
	Note   string        `json:"note,omitempty"`
}

// ReviewDraft is a review to publish on a pull request. CommitSHA pins it to the commit
// that was reviewed, so it cannot appear to be about a newer one.
type ReviewDraft struct {
	IdempotencyKey string
	Verdict        ReviewVerdict
	Body           string
	Comments       []InlineComment
	CommitSHA      string
}

// ReviewThread is a remark found on a pull request.
type ReviewThread struct {
	ID       string `json:"id"`
	Path     string `json:"path,omitempty"`
	Line     int    `json:"line,omitempty"`
	Body     string `json:"body"`
	Author   Actor  `json:"author"`
	Resolved bool   `json:"resolved"`
	// ThreadID is the ID of the remark that started the thread this one belongs to.
	ThreadID string `json:"thread_id,omitempty"`
	// CommitSHA is the commit the remark was written on.
	CommitSHA string `json:"commit_sha,omitempty"`
	// Outdated means the lines it was about no longer exist at the head.
	Outdated bool `json:"outdated,omitempty"`
	// Own is set on those this integration itself posted.
	Own bool `json:"own,omitempty"`
}

// Review is one review submitted on a pull request.
type Review struct {
	ID        string        `json:"id"`
	Author    Actor         `json:"author"`
	Verdict   ReviewVerdict `json:"verdict"`
	Body      string        `json:"body,omitempty"`
	CommitSHA string        `json:"commit_sha,omitempty"`
	// Dismissed reviews no longer hold anything up.
	Dismissed bool `json:"dismissed,omitempty"`
	Own       bool `json:"own,omitempty"`
	// Permission is the author's access, filled in by the orchestrator, never by a provider.
	Permission Permission `json:"permission,omitempty"`
}

// BranchProtection is what an SCM enforces on a branch.
type BranchProtection struct {
	Enabled           bool     `json:"enabled"`
	RequiredChecks    []string `json:"required_checks,omitempty"`
	RequiredApprovals int      `json:"required_approvals,omitempty"`
}

// FindingStatus is where a finding stands.
type FindingStatus string

const (
	// FindingOpen waits for something to happen about it.
	FindingOpen FindingStatus = "open"
	// FindingAddressed went to the implementer. That says nothing about being fixed.
	FindingAddressed FindingStatus = "addressed"
	// FindingStale was written on an earlier commit, and no longer applies to the head.
	FindingStale FindingStatus = "stale"
	// FindingResolved was marked as resolved on the SCM, by whoever may do that there.
	FindingResolved FindingStatus = "resolved"
	// FindingNeedsFix was judged valid, and awaits a correction.
	FindingNeedsFix FindingStatus = "needs-fix"
	// FindingAwaitingReviewer has a question to its reviewer in its thread.
	FindingAwaitingReviewer FindingStatus = "awaiting-reviewer"
	// FindingDisputed has an explanation of why it seems wrong, awaiting the reviewer's view.
	FindingDisputed FindingStatus = "disputed"
	// FindingSuperseded no longer applies, for the reason its history records.
	FindingSuperseded FindingStatus = "superseded"
	// FindingFixedPending has a correction, linked to its commit and evidence. Only the
	// reviewer's own acknowledgement on the SCM takes it further.
	FindingFixedPending FindingStatus = "fixed-pending-confirmation"
)

// GateState says what a pull request is waiting for.
type GateState string

const (
	GateWaiting   GateState = "waiting"
	GateReady     GateState = "ready"
	GateAutoMerge GateState = "auto_merge"
)

// MergeGate is the outcome of a project's merge policy on one commit.
type MergeGate struct {
	SHA   string    `json:"sha"`
	State GateState `json:"state"`
	// Reasons say what stands in the way, in words for a person.
	Reasons []string `json:"reasons,omitempty"`
	// Categories are the sensitive areas the change touches.
	Categories []string `json:"categories,omitempty"`
	// Rule is the expressly approved rule allowing an automatic merge.
	Rule string `json:"rule,omitempty"`
}

// CheckState is the canonical state of a status check.
type CheckState string

const (
	CheckPending CheckState = "pending"
	CheckPassed  CheckState = "passed"
	CheckFailed  CheckState = "failed"
)

// StatusCheck is one CI signal on a commit.
type StatusCheck struct {
	Name  string     `json:"name"`
	State CheckState `json:"state"`
	URL   string     `json:"url,omitempty"`
}
