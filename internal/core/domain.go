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
	Author  Actor   `json:"author"`
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

// ReviewDraft is a review to publish on a pull request.
type ReviewDraft struct {
	IdempotencyKey string
	Verdict        ReviewVerdict
	Body           string
	Comments       []InlineComment
}

// ReviewThread is a remark found on a pull request.
type ReviewThread struct {
	ID       string `json:"id"`
	Path     string `json:"path,omitempty"`
	Line     int    `json:"line,omitempty"`
	Body     string `json:"body"`
	Author   Actor  `json:"author"`
	Resolved bool   `json:"resolved"`
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
