package core

import "time"

// EventType enumerates canonical, provider-independent events.
type EventType string

const (
	EventIssueOpened     EventType = "issue.opened"
	EventIssueEdited     EventType = "issue.edited"
	EventIssueLabeled    EventType = "issue.labeled"
	EventIssueClosed     EventType = "issue.closed"
	EventCommentCreated  EventType = "comment.created"
	EventCommentEdited   EventType = "comment.edited"
	EventPROpened        EventType = "pr.opened"
	EventPRSynchronized  EventType = "pr.synchronized"
	EventPRReopened      EventType = "pr.reopened"
	EventPRClosed        EventType = "pr.closed"
	EventPRReviewed      EventType = "pr.reviewed"
	EventReviewComment   EventType = "pr.review_comment"
	EventCheckCompleted  EventType = "check.completed"
	EventJobFinished     EventType = "job.finished"
	EventJobStarted      EventType = "job.started"
	EventTimerFired      EventType = "timer.fired"
	EventOperatorCommand EventType = "operator.command"
)

// Event is an immutable envelope around a normalized signal. DedupKey
// identifies the fact it reports, not the delivery: the same fact observed by a
// webhook, a redelivery and a poll carries one key, so it is processed once.
type Event struct {
	Seq        int64     `json:"seq,omitempty"` // assigned by the store
	Project    string    `json:"project"`
	Source     string    `json:"source"` // event source instance, or "engine"
	Type       EventType `json:"type"`
	Subject    Subject   `json:"subject"`
	DedupKey   string    `json:"dedup_key"`
	DeliveryID string    `json:"delivery_id,omitempty"`
	Actor      Actor     `json:"actor"`
	OccurredAt time.Time `json:"occurred_at"`
	ReceivedAt time.Time `json:"received_at"`

	Issue       *Issue        `json:"issue,omitempty"`
	Comment     *Comment      `json:"comment,omitempty"`
	PullRequest *PullRequest  `json:"pull_request,omitempty"`
	Label       string        `json:"label,omitempty"`
	Check       *StatusCheck  `json:"check,omitempty"`
	Verdict     ReviewVerdict `json:"verdict,omitempty"`
	Job         *JobOutcome   `json:"job,omitempty"`
}

// WebhookRequest is a raw inbound delivery. Implementations must authenticate
// it before trusting any field.
type WebhookRequest struct {
	Headers map[string][]string
	Body    []byte
}

// Header returns the first value of a header, case-insensitively.
func (r WebhookRequest) Header(name string) string {
	for k, v := range r.Headers {
		if len(v) > 0 && equalFold(k, name) {
			return v[0]
		}
	}
	return ""
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// PollRequest asks a source for signals newer than Cursor. An empty cursor
// starts from Since.
type PollRequest struct {
	Container string
	Cursor    string
	Since     time.Time
}

// PollResult returns events and the cursor to persist after they are stored.
type PollResult struct {
	Events     []Event
	NextCursor string
}
