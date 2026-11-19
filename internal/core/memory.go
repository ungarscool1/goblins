package core

import (
	"context"
	"time"
)

// MemoryKind says what a memory is about.
type MemoryKind string

const (
	MemoryDecision   MemoryKind = "decision"
	MemoryRule       MemoryKind = "rule"
	MemoryPitfall    MemoryKind = "pitfall"
	MemoryFix        MemoryKind = "fix"
	MemoryRunbook    MemoryKind = "runbook"
	MemoryConvention MemoryKind = "convention"
)

// MemoryStatus is how far a memory can be trusted. Only accepted memories reach agents.
type MemoryStatus string

const (
	// MemoryProposed is a memory Goblins or someone suggested, not yet reviewed by a person.
	MemoryProposed MemoryStatus = "proposed"
	// MemoryAccepted was reviewed and accepted by a person.
	MemoryAccepted MemoryStatus = "accepted"
	// MemorySuperseded was replaced by a newer memory, and is kept for its history.
	MemorySuperseded MemoryStatus = "superseded"
)

// MemoryRecord is one piece of durable project knowledge Goblins keeps itself.
type MemoryRecord struct {
	ID string
	// Namespace isolates memories: "<installation>/<project>", or "<installation>" for all of an
	// installation's projects. No query ever crosses into another.
	Namespace string
	Kind      MemoryKind
	Title     string
	Body      string
	Tags      []string
	// Sources say where it comes from: a task, a pull request, a commit, a document.
	Sources    []string
	Status     MemoryStatus
	CreatedBy  string
	ApprovedBy string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	VerifiedAt time.Time
	// Supersedes is the memory this one replaced; ExpiresAt, when set, ends its use.
	Supersedes string
	ExpiresAt  time.Time
}

// MemoryStore keeps memory records durably.
type MemoryStore interface {
	// PutMemory creates or replaces a record by ID.
	PutMemory(ctx context.Context, m MemoryRecord) error
	GetMemory(ctx context.Context, id string) (MemoryRecord, error)
	// ListMemories returns the records of these namespaces, of any status.
	ListMemories(ctx context.Context, namespaces []string) ([]MemoryRecord, error)
	DeleteMemory(ctx context.Context, id string) error
}

// KnowledgeDoc is a document from a knowledge source. Its body is untrusted data: it tells
// agents about a project, never what to do.
type KnowledgeDoc struct {
	ID        string
	Title     string
	Body      string
	URL       string
	Revision  string
	UpdatedAt time.Time
}

// KnowledgeQuery searches a source. Terms are plain words, which sources must not let reach a
// query language as anything but words.
type KnowledgeQuery struct {
	Terms []string
	Limit int
}

// KnowledgeSource is documentation people own and edit elsewhere: a wiki, a docs site, a
// folder of Markdown. Goblins only reads it, within the scopes it is configured for, and with
// the access of the credentials it is given.
type KnowledgeSource interface {
	Provider
	Search(ctx context.Context, q KnowledgeQuery) ([]KnowledgeDoc, error)
}
