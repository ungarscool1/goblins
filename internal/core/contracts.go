package core

import (
	"context"
	"io"
	"time"
)

// IssueTracker is a system of record for work items and their conversation.
type IssueTracker interface {
	Provider
	GetIssue(ctx context.Context, ref IssueRef) (Issue, error)
	ListIssues(ctx context.Context, q IssueQuery) ([]Issue, error)
	ListComments(ctx context.Context, ref IssueRef, since time.Time) ([]Comment, error)
	// PostComment is idempotent on draft.IdempotencyKey.
	// Requires CapTrackerCommentWrite.
	PostComment(ctx context.Context, ref IssueRef, draft CommentDraft) (Comment, error)
	// ActorPermission returns the actor's effective access on the issue's
	// container. Requires CapTrackerPermissions. Callers treat an error as no
	// permission: authorization always fails closed.
	ActorPermission(ctx context.Context, ref IssueRef, actor Actor) (Permission, error)
	// SetLabels requires CapTrackerLabels.
	SetLabels(ctx context.Context, ref IssueRef, add, remove []string) error
	// Transition moves the item to a tracker-specific status. Requires
	// CapTrackerTransitions.
	Transition(ctx context.Context, ref IssueRef, status string) error
}

// SourceControl hosts repositories. It is separate from IssueTracker because
// one installation may combine any tracker with any SCM.
type SourceControl interface {
	Provider
	// GitAccess returns credentials scoped as narrowly as the SCM allows.
	GitAccess(ctx context.Context, repo RepoRef, write bool) (GitAccess, error)
	ResolveBranch(ctx context.Context, repo RepoRef, branch string) (sha string, err error)
	GetCommit(ctx context.Context, repo RepoRef, sha string) (Commit, error)
	// EnsurePullRequest creates the PR for draft.Head or updates the existing
	// one. It never opens a second PR for one head. Requires CapSCMPullRequests,
	// and CapSCMDraft when draft.Draft is set.
	EnsurePullRequest(ctx context.Context, draft PullRequestDraft) (pr PullRequest, created bool, err error)
	GetPullRequest(ctx context.Context, repo RepoRef, number string) (PullRequest, error)
	// PostPullRequestComment is idempotent on draft.IdempotencyKey.
	PostPullRequestComment(ctx context.Context, repo RepoRef, number string, draft CommentDraft) error
	// PostReview requires CapSCMInlineReview when the draft has inline comments.
	PostReview(ctx context.Context, repo RepoRef, number string, draft ReviewDraft) error
	ReviewThreads(ctx context.Context, repo RepoRef, number string) ([]ReviewThread, error)
	// StatusChecks requires CapSCMStatusChecks.
	StatusChecks(ctx context.Context, repo RepoRef, sha string) ([]StatusCheck, error)
	// ListReviews returns every review submitted on a pull request, oldest first.
	ListReviews(ctx context.Context, repo RepoRef, number string) ([]Review, error)
	// ChangedFiles returns the paths a pull request changes.
	ChangedFiles(ctx context.Context, repo RepoRef, number string) ([]string, error)
	// BranchProtection reports what the SCM enforces on a branch. Requires
	// CapSCMBranchProtection.
	BranchProtection(ctx context.Context, repo RepoRef, branch string) (BranchProtection, error)
	// MergePullRequest merges exactly sha, and fails with KindConflict if the head has moved.
	// Requires CapSCMMerge.
	MergePullRequest(ctx context.Context, repo RepoRef, number, sha string) error
}

// EventSource normalizes external signals into canonical events. It reports
// facts only: deciding what they mean belongs to the workflow.
type EventSource interface {
	Provider
	// HandleWebhook authenticates a delivery and normalizes it. Requires
	// CapEventsWebhook. A delivery that is valid but irrelevant yields no events.
	HandleWebhook(ctx context.Context, req WebhookRequest) ([]Event, error)
	// Poll requires CapEventsPoll.
	Poll(ctx context.Context, req PollRequest) (PollResult, error)
}

// AgentRole is the purpose of an agent run. It decides the tool policy.
type AgentRole string

const (
	RolePlanner     AgentRole = "planner"
	RoleImplementer AgentRole = "implementer"
	RoleReviewer    AgentRole = "reviewer"
)

// ToolPolicy is the abstract permission set of a run; adapters map it to their
// tool names. There is deliberately no way to grant command execution: an agent
// run holds subscription credentials, so running repository code is left to
// credential-free workloads.
type ToolPolicy struct {
	WriteFiles bool
	Web        bool
}

// AgentRequest launches a new conversation or, with ResumeSessionID, resumes one.
type AgentRequest struct {
	IdempotencyKey string
	// WorkloadName is unique per attempt, so a retry never attaches to the workload
	// of a dead attempt; orphan reconciliation removes that one.
	WorkloadName    string
	Role            AgentRole
	Prompt          string
	SystemPrompt    string
	Model           string
	Effort          string
	ResumeSessionID string
	Policy          ToolPolicy
	Runtime         ExecutionRuntime
	Workspace       Mount
	WorkDir         string
	Timeout         time.Duration
	Labels          map[string]string
}

// AgentEventKind classifies stream entries.
type AgentEventKind string

const (
	AgentEventSession AgentEventKind = "session"
	AgentEventText    AgentEventKind = "text"
	AgentEventTool    AgentEventKind = "tool"
	AgentEventResult  AgentEventKind = "result"
)

// AgentEvent is one normalized stream entry.
type AgentEvent struct {
	Kind      AgentEventKind `json:"kind"`
	SessionID string         `json:"session_id,omitempty"`
	Text      string         `json:"text,omitempty"`
}

// AgentResult is the outcome of a finished run.
type AgentResult struct {
	SessionID string
	Output    string
}

// AuthState is the login state of an agent installation.
type AuthState string

const (
	AuthOK            AuthState = "ok"
	AuthLoginRequired AuthState = "login_required"
	AuthUnknown       AuthState = "unknown"
)

// AuthStatus reports AuthState with a user-actionable detail.
type AuthStatus struct {
	State  AuthState
	Detail string
}

// Model is a model the agent can be asked to use.
type Model struct {
	ID          string
	Description string
	// Efforts lists the efforts the model accepts, when the agent can tell.
	Efforts []string
}

// AgentProvider runs a coding agent conversation.
type AgentProvider interface {
	Provider
	// Run launches or resumes a conversation in req.Runtime and blocks until it
	// ends. Cancelling ctx cancels the run. Events are delivered to sink in
	// order, and the session ID is delivered as soon as it is known so a crashed
	// run can be resumed. Exhausted quota and missing login are returned as
	// KindQuotaExhausted and KindAuthRequired.
	Run(ctx context.Context, req AgentRequest, sink func(AgentEvent)) (AgentResult, error)
	AuthStatus(ctx context.Context, rt ExecutionRuntime) (AuthStatus, error)
	Models(ctx context.Context) ([]Model, error)
}

// Mount attaches a named volume to a workload.
type Mount struct {
	Volume   string
	Target   string
	ReadOnly bool
}

// NetworkPolicy restricts workload connectivity.
type NetworkPolicy string

const (
	NetworkNone   NetworkPolicy = "none"
	NetworkEgress NetworkPolicy = "egress"
)

// Resources bounds a workload. Zero means the runtime's configured default.
type Resources struct {
	CPUs        float64
	MemoryBytes int64
	DiskBytes   int64
	PIDs        int64
}

// SecurityProfile requests features beyond the unprivileged default. Runtimes
// must reject a spec asking for something they do not advertise.
type SecurityProfile struct {
	// ReadOnlyRoot makes the image filesystem read-only; only mounts and a
	// scratch /tmp are writable.
	ReadOnlyRoot bool
	Privileged   bool
	GPU          bool
	KVM          bool
}

// WorkloadSpec describes one isolated process.
type WorkloadSpec struct {
	Name      string // unique; creating an existing name returns that workload
	Image     string
	Command   []string
	WorkDir   string
	Env       map[string]string
	Secrets   map[string]Secret // injected as environment, never logged
	Mounts    []Mount
	Network   NetworkPolicy
	Resources Resources
	Security  SecurityProfile
	Timeout   time.Duration
	Labels    map[string]string
}

// CheckWorkloadSpec rejects a spec needing capabilities rt does not advertise. All
// runtimes share it so they refuse the same way, before starting anything.
func CheckWorkloadSpec(rt ExecutionRuntime, spec WorkloadSpec) error {
	var req []Capability
	if spec.Security.Privileged {
		req = append(req, CapRuntimePrivileged)
	}
	if spec.Security.GPU {
		req = append(req, CapRuntimeGPU)
	}
	if spec.Security.KVM {
		req = append(req, CapRuntimeKVM)
	}
	if spec.Security.ReadOnlyRoot {
		req = append(req, CapRuntimeReadOnlyRoot)
	}
	if spec.Resources.DiskBytes > 0 {
		req = append(req, CapRuntimeDiskLimits)
	}
	if spec.Resources != (Resources{}) {
		req = append(req, CapRuntimeResourceLimits)
	}
	switch spec.Network {
	case NetworkNone:
		req = append(req, CapRuntimeNetworkNone)
	case NetworkEgress:
		req = append(req, CapRuntimeNetworkEgress)
	default:
		return Errorf(KindInvalid, "runtime.create", "workload %s has no network policy", spec.Name)
	}
	if len(spec.Mounts) > 0 {
		req = append(req, CapRuntimeVolumes)
	}
	return Negotiate(rt, req...)
}

// WorkloadPhase is the lifecycle of a workload.
type WorkloadPhase string

const (
	WorkloadPending   WorkloadPhase = "pending"
	WorkloadRunning   WorkloadPhase = "running"
	WorkloadSucceeded WorkloadPhase = "succeeded"
	WorkloadFailed    WorkloadPhase = "failed"
	WorkloadCancelled WorkloadPhase = "cancelled"
	WorkloadTimedOut  WorkloadPhase = "timed_out"
)

// Terminal reports whether the phase is final.
func (p WorkloadPhase) Terminal() bool {
	return p != WorkloadPending && p != WorkloadRunning
}

// WorkloadStatus reports a workload's state.
type WorkloadStatus struct {
	Name     string
	Phase    WorkloadPhase
	ExitCode int
	Labels   map[string]string
}

// ExecutionRuntime runs isolated workloads, wherever they live.
type ExecutionRuntime interface {
	Provider
	// Create starts a workload. It is idempotent on spec.Name. A spec requesting
	// an unadvertised capability fails with KindUnsupported and starts nothing.
	Create(ctx context.Context, spec WorkloadSpec) (WorkloadStatus, error)
	Status(ctx context.Context, name string) (WorkloadStatus, error)
	// Logs copies output to the writers. With follow it blocks until the
	// workload ends or ctx is cancelled.
	Logs(ctx context.Context, name string, follow bool, stdout, stderr io.Writer) error
	// Cancel stops a workload. Cancelling a finished workload is a no-op.
	Cancel(ctx context.Context, name string) error
	// Cleanup removes a workload and its temporary resources. It is idempotent.
	Cleanup(ctx context.Context, name string) error
	// List returns workloads carrying all the labels, for orphan reconciliation.
	List(ctx context.Context, labels map[string]string) ([]WorkloadStatus, error)
	// EnsureVolume creates a durable volume if needed. RemoveVolume is idempotent.
	EnsureVolume(ctx context.Context, name string, labels map[string]string) error
	RemoveVolume(ctx context.Context, name string) error
}

// ReviewRequest asks for an independent review of a pull request at one exact commit.
// The workspace fields are set only for providers advertising CapReviewWorkspace: a
// read-only checkout of HeadSHA, and the diff against the base as data.
type ReviewRequest struct {
	IdempotencyKey string
	Repo           RepoRef
	PullRequest    PullRequest
	HeadSHA        string
	Design         string
	Diff           string
	Runtime        ExecutionRuntime
	Workspace      Mount
	WorkDir        string
	WorkloadName   string
	Labels         map[string]string
}

// ReviewResult reports a review. VerdictSkipped must say why in Summary. It is an opinion:
// nothing about it proves that a change is safe, and the workflow never treats it so.
type ReviewResult struct {
	Verdict  ReviewVerdict   `json:"verdict"`
	Summary  string          `json:"summary"`
	Findings []ReviewFinding `json:"findings,omitempty"`
	// Provider and PromptVersion say what produced this, for provenance.
	Provider      string `json:"provider,omitempty"`
	PromptVersion string `json:"prompt_version,omitempty"`
	// Published means the reviewer has already put it on the pull request itself.
	Published bool `json:"published,omitempty"`
}

// ReviewProvider reviews pull requests independently of the implementer.
type ReviewProvider interface {
	Provider
	Review(ctx context.Context, req ReviewRequest) (ReviewResult, error)
}

// ValidationStatus is the outcome of deterministic checks.
type ValidationStatus string

const (
	ValidationPassed ValidationStatus = "passed"
	ValidationFailed ValidationStatus = "failed"
	// ValidationError means checks could not run; it says nothing about the code.
	ValidationError ValidationStatus = "error"
	// ValidationSkipped means a project chose to run no checks. It is never a pass.
	ValidationSkipped ValidationStatus = "skipped"
)

// ValidationCheck is one executed check.
type ValidationCheck struct {
	Name     string           `json:"name"`
	Status   ValidationStatus `json:"status"`
	Output   string           `json:"output,omitempty"`
	ExitCode int              `json:"exit_code"`
}

// ValidationRequest asks to run a project's checks on a workspace. Workloads
// started for it must not receive agent or SCM credentials.
type ValidationRequest struct {
	IdempotencyKey string
	WorkloadName   string
	HeadSHA        string
	Workspace      Mount
	WorkDir        string
	Runtime        ExecutionRuntime
	Labels         map[string]string
}

// ValidationReport aggregates check results.
type ValidationReport struct {
	Status  ValidationStatus  `json:"status"`
	Summary string            `json:"summary"`
	Checks  []ValidationCheck `json:"checks,omitempty"`
}

// ValidationProvider runs deterministic checks; its verdict gates the workflow.
type ValidationProvider interface {
	Provider
	Validate(ctx context.Context, req ValidationRequest) (ValidationReport, error)
}

// Severity grades a notification.
type Severity string

const (
	SeverityInfo   Severity = "info"
	SeverityAction Severity = "action_required"
	SeverityError  Severity = "error"
)

// Notification tells an operator something outside the ticket conversation.
type Notification struct {
	IdempotencyKey string
	Project        string
	Subject        Subject
	Severity       Severity
	Title          string
	Body           string
}

// NotificationProvider delivers operator notifications. Delivery is idempotent
// on Notification.IdempotencyKey.
type NotificationProvider interface {
	Provider
	Notify(ctx context.Context, n Notification) error
}
