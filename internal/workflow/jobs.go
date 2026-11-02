// Package workflow is the deterministic core of Goblins. Decide is a pure
// function from a task and an event to the next state and the jobs to run; model
// output and repository content reach it only as data, so they cannot move a task
// past a human gate.
package workflow

import (
	"encoding/json"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

// Job kinds requested by the state machine.
const (
	JobPlan      core.JobKind = "plan"
	JobImplement core.JobKind = "implement"
	JobValidate  core.JobKind = "validate"
	JobPublish   core.JobKind = "publish"
	JobReview    core.JobKind = "review"
	JobReconcile core.JobKind = "reconcile"
	JobMerge     core.JobKind = "merge"
	JobComment   core.JobKind = "comment"
	JobNotify    core.JobKind = "notify"
	JobCleanup   core.JobKind = "cleanup"
)

// mainKinds are the jobs that advance a task; only one is current at a time.
var mainKinds = []core.JobKind{JobPlan, JobImplement, JobValidate, JobPublish, JobReview,
	JobReconcile, JobMerge}

// PlanPayload parametrizes a planner run.
type PlanPayload struct {
	Trigger string `json:"trigger"`
}

// PlanResult is what a planner job reports. Exactly one of Questions or Design
// is set. It is untrusted model output, validated by the job handler.
type PlanResult struct {
	SessionID string   `json:"session_id"`
	Questions []string `json:"questions,omitempty"`
	Design    string   `json:"design,omitempty"`
}

// ImplementPayload parametrizes an implementer run.
type ImplementPayload struct {
	Revision int    `json:"revision"`
	Hash     string `json:"hash"`
	Feedback string `json:"feedback,omitempty"`
	// ReviewFeedback carries a review's actionable findings, for a correction round.
	ReviewFeedback string `json:"review_feedback,omitempty"`
}

// ImplementResult reports an implementer run.
type ImplementResult struct {
	SessionID string `json:"session_id"`
	Workspace string `json:"workspace"`
	Branch    string `json:"branch"`
	BaseSHA   string `json:"base_sha"`
	HeadSHA   string `json:"head_sha"`
	Changed   bool   `json:"changed"`
}

// ValidateStage says where in the workflow validation runs.
type ValidateStage string

const (
	StagePrePR ValidateStage = "pre_pr"
	StageFinal ValidateStage = "final"
)

// ValidatePayload parametrizes a validation run.
type ValidatePayload struct {
	Stage ValidateStage `json:"stage"`
	SHA   string        `json:"sha"`
	// Reuse reports the earlier pass at this exact commit instead of re-running.
	Reuse bool `json:"reuse,omitempty"`
}

// ValidateResult reports a validation run.
type ValidateResult struct {
	Report core.ValidationReport `json:"report"`
	// Skipped means the project configures no validation. It is reported as such.
	Skipped bool `json:"skipped,omitempty"`
}

// PublishPayload parametrizes publishing a commit.
type PublishPayload struct {
	SHA      string `json:"sha"`
	Revision int    `json:"revision"`
}

// PublishResult reports a published branch and pull request.
type PublishResult struct {
	PullRequest core.PullRequest `json:"pull_request"`
	Created     bool             `json:"created"`
}

// ReviewPromptVersion pins what reviews are asked. A review is done once per commit and
// version, and every published review says which version produced it. Changing the prompt
// means changing this; a test holds the two together.
const ReviewPromptVersion = "2026-11-01.1"

// ReviewPayload parametrizes a review. SHA is the one commit to review.
type ReviewPayload struct {
	SHA string `json:"sha,omitempty"`
}

// ReviewResult reports a review; Skipped reviews say why in Result.Summary.
type ReviewResult struct {
	Result core.ReviewResult `json:"result"`
	SHA    string            `json:"sha,omitempty"`
}

// ReconcileResult is what an SCM says about a pull request right now, whatever event led to
// asking. Only people and reviewers with sufficient access are in it, and everything in it
// is data.
type ReconcileResult struct {
	PullRequest core.PullRequest       `json:"pull_request"`
	Findings    []core.ReviewFinding   `json:"findings,omitempty"`
	Reviews     []core.Review          `json:"reviews,omitempty"`
	Checks      []core.StatusCheck     `json:"checks,omitempty"`
	Files       []string               `json:"files,omitempty"`
	Protection  *core.BranchProtection `json:"protection,omitempty"`
	// CanMerge says the SCM can merge at all.
	CanMerge bool `json:"can_merge,omitempty"`
}

// MergePayload parametrizes merging one exact commit.
type MergePayload struct {
	SHA  string `json:"sha"`
	Rule string `json:"rule"`
}

// CommentPayload is a message for the ticket conversation.
type CommentPayload struct {
	Body string `json:"body"`
}

// CommentResult reports where a message went. PostedAt is the tracker's own timestamp,
// comparable to the timestamps on people's replies regardless of clock skew. It is unset
// when the message went through another channel.
type CommentResult struct {
	Via      string    `json:"via"`
	PostedAt time.Time `json:"posted_at,omitzero"`
}

// NotifyPayload is a message for operators.
type NotifyPayload struct {
	Severity core.Severity `json:"severity"`
	Title    string        `json:"title"`
	Body     string        `json:"body"`
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // payloads are plain structs defined above
	}
	return b
}

// EmitsStart reports whether a job's start matters to the state machine.
func EmitsStart(k core.JobKind) bool { return k == JobImplement || k == JobReview }

// SideJob reports whether a job only performs an effect and never advances a task.
func SideJob(k core.JobKind) bool { return k == JobComment || k == JobNotify || k == JobCleanup }
