package core

import (
	"context"
	"time"
)

// Lease gives one holder, such as a task, exclusive use of a resource, such as a shared
// development environment, until it expires. Data is the holder's notes on the resource, kept
// with the lease so they survive restarts.
type Lease struct {
	Resource   string
	Holder     string
	AcquiredAt time.Time
	ExpiresAt  time.Time
	Data       map[string]string
}

// LeaseStore keeps leases durably, and atomically across processes.
type LeaseStore interface {
	// AcquireLease gives l.Resource to l.Holder until l.ExpiresAt, or extends the holder's own
	// lease, keeping its data. A lease of another holder that has not expired at now is left
	// alone: the result is that lease, with KindConflict.
	AcquireLease(ctx context.Context, l Lease, now time.Time) (Lease, error)
	GetLease(ctx context.Context, resource string) (Lease, error)
	// UpdateLease replaces the expiry and data of the holder's own lease, or yields
	// KindConflict if the holder no longer has it.
	UpdateLease(ctx context.Context, l Lease) error
	// ReleaseLease ends the holder's lease. Releasing one the holder does not have is a no-op.
	ReleaseLease(ctx context.Context, resource, holder string) error
	ListLeases(ctx context.Context, prefix string) ([]Lease, error)
}
