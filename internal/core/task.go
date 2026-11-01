package core

import (
	"context"
	"encoding/json"
	"time"
)

// State is a task's position in the workflow.
type State string

const (
	StateDiscovered     State = "discovered"
	StateInvestigating  State = "investigating"
	StateWaitingForUser State = "waiting-for-user"
	StateDesignProposed State = "design-proposed"
	StateAgentReady     State = "agent-ready"
	StateImplementing   State = "implementing"
	StatePROpen         State = "pr-open"
	StateReviewing      State = "reviewing"
	StateTesting        State = "testing"
	StateCompleted      State = "completed"
	StateBlocked        State = "blocked"
	StateFailed         State = "failed"
	StateCancelled      State = "cancelled"
	StateQuotaPaused    State = "quota-paused"
)

// TaskKind distinguishes full issue workflows from review-only ones.
type TaskKind string

const (
	TaskIssue      TaskKind = "issue"
	TaskReviewOnly TaskKind = "review-only"
)

// Approval records who approved which exact design.
type Approval struct {
	Revision  int       `json:"revision"`
	Hash      string    `json:"hash"`
	Approver  Actor     `json:"approver"`
	CommentID string    `json:"comment_id"`
	At        time.Time `json:"at"`
}

// Design is one immutable proposed revision.
type Design struct {
	TaskID    string    `json:"task_id"`
	Revision  int       `json:"revision"`
	Hash      string    `json:"hash"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// Task is the durable record of one work item. Only the workflow engine
// changes it, and only through StateStore.Commit.
type Task struct {
	ID      string   `json:"id"`
	Project string   `json:"project"`
	Kind    TaskKind `json:"kind"`
	Subject Subject  `json:"subject"`
	State   State    `json:"state"`
	Version int64    `json:"version"`

	Title            string `json:"title,omitempty"`
	RequirementsHash string `json:"requirements_hash,omitempty"`

	// DesignCommentKey identifies the comment that published the current revision.
	DesignCommentKey string    `json:"design_comment_key,omitempty"`
	DesignRevision   int       `json:"design_revision,omitempty"`
	DesignHash       string    `json:"design_hash,omitempty"`
	DesignProposedAt time.Time `json:"design_proposed_at,omitzero"`
	Approval         *Approval `json:"approval,omitempty"`

	Sessions map[AgentRole]string `json:"sessions,omitempty"`

	Branch       string `json:"branch,omitempty"`
	BaseSHA      string `json:"base_sha,omitempty"`
	HeadSHA      string `json:"head_sha,omitempty"`
	ValidatedSHA string `json:"validated_sha,omitempty"`
	// ReviewedKey identifies the commit and review prompt version last reviewed, so that
	// nothing reviews the same thing twice.
	ReviewedKey string `json:"reviewed_key,omitempty"`
	// CorrectionFeedback is what the review asked of the implementer in the current round.
	CorrectionFeedback string       `json:"correction_feedback,omitempty"`
	PullRequest        *PullRequest `json:"pull_request,omitempty"`
	Workspace          string       `json:"workspace,omitempty"`

	// Loops counts bounded iterations per loop name; the engine enforces limits.
	Loops map[string]int `json:"loops,omitempty"`

	// CurrentJob is the main job in flight. Only its outcome may advance the
	// task, and leaving a pause re-enqueues it under a fresh key.
	CurrentJob *JobDraft `json:"current_job,omitempty"`
	// PendingInput records input that arrived while a planner job was running,
	// so its result is superseded instead of being posted.
	PendingInput bool `json:"pending_input,omitempty"`

	// ManualInput holds replies given through the manual inbox, which trackers never saw,
	// so that the planner can read them. Only the newest are kept.
	ManualInput []Comment `json:"manual_input,omitempty"`
	// ResumeState is the state to return to when leaving a pause or block.
	ResumeState State     `json:"resume_state,omitempty"`
	PausedUntil time.Time `json:"paused_until,omitzero"`
	Reason      string    `json:"reason,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Transition is an append-only audit record of a state change.
type Transition struct {
	TaskID   string    `json:"task_id"`
	From     State     `json:"from"`
	To       State     `json:"to"`
	Reason   string    `json:"reason"`
	EventSeq int64     `json:"event_seq"`
	At       time.Time `json:"at"`
}

// JobKind names a unit of side-effecting work.
type JobKind string

// JobState is the lifecycle of a job.
type JobState string

const (
	JobPending   JobState = "pending"
	JobRunning   JobState = "running"
	JobSucceeded JobState = "succeeded"
	JobFailed    JobState = "failed"
	JobCancelled JobState = "cancelled"
)

// JobDraft is a job the workflow wants done.
type JobDraft struct {
	Kind           JobKind         `json:"kind"`
	IdempotencyKey string          `json:"idempotency_key"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	Priority       int             `json:"priority,omitempty"`
	MaxAttempts    int             `json:"max_attempts,omitempty"`
	// LimitKey groups jobs sharing a scarce resource such as an agent account.
	LimitKey  string    `json:"limit_key,omitempty"`
	NotBefore time.Time `json:"not_before,omitzero"`
}

// Job is a durable, leased unit of work.
type Job struct {
	JobDraft
	ID         string          `json:"id"`
	TaskID     string          `json:"task_id"`
	Project    string          `json:"project"`
	State      JobState        `json:"state"`
	Attempt    int             `json:"attempt"`
	LeaseOwner string          `json:"lease_owner,omitempty"`
	LeaseUntil time.Time       `json:"lease_until,omitzero"`
	Checkpoint json.RawMessage `json:"checkpoint,omitempty"`
	LastError  string          `json:"last_error,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

// JobOutcome is reported back to the workflow as an event.
type JobOutcome struct {
	JobID     string          `json:"job_id"`
	Kind      JobKind         `json:"kind"`
	Key       string          `json:"key"`
	OK        bool            `json:"ok"`
	ErrorKind ErrorKind       `json:"error_kind,omitempty"`
	Message   string          `json:"message,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
}

// StateCommit is the atomic result of processing one event. It succeeds entirely or
// not at all, which is what gives effects their exactly-once-by-idempotency
// behaviour: an event is marked processed in the same transaction that records
// the transition and enqueues its jobs.
type StateCommit struct {
	EventSeq        int64
	EventOwner      string
	Task            *Task // nil when the event changes nothing
	ExpectedVersion int64 // 0 inserts a new task
	Transition      *Transition
	Design          *Design
	Jobs            []JobDraft
	// CancelKinds cancels pending and running jobs of Task with these kinds.
	CancelKinds []JobKind
	Now         time.Time
}

// JobClaim requests the next runnable job under concurrency limits.
//
// Limits apply to heavy jobs, those with a LimitKey. Light jobs such as posting a
// comment are never starved by long agent runs. Zero means unlimited.
type JobClaim struct {
	Owner         string
	LeaseFor      time.Duration
	Now           time.Time
	MaxGlobal     int
	MaxPerProject int
	MaxPerLimit   map[string]int
	// MinPriority restricts the claim to jobs of at least this priority, so that workers
	// reserved for quick, urgent jobs never get stuck with long ones.
	MinPriority int
}

// JobFinish records a job's end together with its outcome event.
type JobFinish struct {
	JobID   string
	Owner   string
	Attempt int
	State   JobState
	Error   string
	Event   *Event
	Now     time.Time
}

// TranscriptEntry is one bounded line of worker output.
type TranscriptEntry struct {
	TaskID string    `json:"task_id"`
	JobID  string    `json:"job_id"`
	Kind   string    `json:"kind"`
	Text   string    `json:"text"`
	At     time.Time `json:"at"`
}

// StateStore persists all durable state. Implementations must make each method
// atomic and safe across concurrent processes.
type StateStore interface {
	// AppendEvent inserts an event unless its DedupKey exists; the second result
	// reports whether it was inserted.
	AppendEvent(ctx context.Context, ev Event) (Event, bool, error)
	// ClaimEvent leases the oldest unprocessed event whose subject has no other
	// leased event, serializing processing per issue or pull request. It returns
	// nil when there is nothing to do.
	ClaimEvent(ctx context.Context, owner string, leaseFor time.Duration, now time.Time) (*Event, error)
	// ReleaseEvent gives up a lease so the event is retried at notBefore.
	ReleaseEvent(ctx context.Context, seq int64, owner string, notBefore time.Time) error
	// Commit applies the result of processing an event. A stale version or lost
	// lease yields KindConflict and changes nothing.
	Commit(ctx context.Context, c StateCommit) error

	GetTask(ctx context.Context, id string) (Task, error)
	FindTask(ctx context.Context, project string, subject Subject) (Task, error)
	ListTasks(ctx context.Context, project string) ([]Task, error)
	// DueTasks lists tasks paused until a time that has passed.
	DueTasks(ctx context.Context, now time.Time) ([]Task, error)
	ListTransitions(ctx context.Context, taskID string) ([]Transition, error)
	GetDesign(ctx context.Context, taskID string, revision int) (Design, error)

	// ClaimJob leases the next runnable job, including jobs whose lease expired.
	// Jobs that exhausted their attempts are finished as failed with an outcome
	// event rather than being handed out again.
	ClaimJob(ctx context.Context, c JobClaim) (*Job, error)
	// HeartbeatJob extends a lease; it returns KindConflict once the lease is
	// lost, or if the job was cancelled, and the worker must stop.
	HeartbeatJob(ctx context.Context, jobID, owner string, attempt int, until time.Time) error
	SaveCheckpoint(ctx context.Context, jobID, owner string, attempt int, data json.RawMessage) error
	// RetryJob returns a running job to the queue at notBefore.
	RetryJob(ctx context.Context, jobID, owner string, attempt int, notBefore time.Time, lastError string) error
	// RequeueInterruptedJob is RetryJob for an attempt stopped through no fault of the
	// job, such as a shutdown. The attempt number still rises, because it is the fencing
	// token, so the job's budget grows by one to keep this from counting as a failure.
	RequeueInterruptedJob(ctx context.Context, jobID, owner string, attempt int, notBefore time.Time, lastError string) error
	// FinishJob ends a job and atomically appends its outcome event. A stale
	// owner or attempt yields KindConflict: results of a fenced worker are dropped.
	FinishJob(ctx context.Context, f JobFinish) error
	GetJob(ctx context.Context, id string) (Job, error)
	ListJobs(ctx context.Context, taskID string) ([]Job, error)

	AppendTranscript(ctx context.Context, entries []TranscriptEntry) error
	ListTranscript(ctx context.Context, taskID string, limit int) ([]TranscriptEntry, error)

	GetCursor(ctx context.Context, name string) (string, error)
	SetCursor(ctx context.Context, name, value string) error

	Ping(ctx context.Context) error
	Close() error
}

// IsWaiting reports whether the task is idle until a person replies.
func (s State) IsWaiting() bool { return s == StateWaitingForUser || s == StateDesignProposed }
