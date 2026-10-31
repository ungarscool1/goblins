// Package core holds the provider-neutral domain model and contracts of
// Goblins. It must never contain behavior specific to a tracker, SCM, model
// vendor or execution backend; internal/archtest enforces that.
package core

import (
	"errors"
	"fmt"
)

// ErrorKind classifies failures so orchestration logic can react to them
// without inspecting provider-specific errors.
type ErrorKind string

const (
	KindInternal       ErrorKind = "internal"
	KindInvalid        ErrorKind = "invalid"
	KindNotFound       ErrorKind = "not_found"
	KindConflict       ErrorKind = "conflict"
	KindUnauthorized   ErrorKind = "unauthorized"
	KindForbidden      ErrorKind = "forbidden"
	KindUnsupported    ErrorKind = "unsupported"
	KindTransient      ErrorKind = "transient"
	KindAuthRequired   ErrorKind = "auth_required"
	KindQuotaExhausted ErrorKind = "quota_exhausted"
	KindTimeout        ErrorKind = "timeout"
	KindCancelled      ErrorKind = "cancelled"
)

// Error is the structured error type used across Goblins. Op names the
// failed operation, Message is safe to show to users and must not contain
// secrets, and Err optionally carries the underlying cause.
type Error struct {
	Kind    ErrorKind
	Op      string
	Message string
	Err     error
}

func (e *Error) Error() string {
	s := string(e.Kind)
	if e.Op != "" {
		s = e.Op + ": " + s
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// Is matches another *Error by kind, so errors.Is(err, &Error{Kind: k}) works.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Kind == e.Kind && t.Op == "" && t.Message == "" && t.Err == nil
}

// Errorf builds an Error with a formatted user-safe message.
func Errorf(kind ErrorKind, op, format string, args ...any) *Error {
	return &Error{Kind: kind, Op: op, Message: fmt.Sprintf(format, args...)}
}

// Wrap attaches a kind and operation to err. A nil err stays nil.
func Wrap(kind ErrorKind, op string, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Kind: kind, Op: op, Err: err}
}

// KindOf returns the kind of the outermost Error in err's chain.
func KindOf(err error) ErrorKind {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindInternal
}

// IsKind reports whether err is classified as kind.
func IsKind(err error, kind ErrorKind) bool { return KindOf(err) == kind }

// UserMessage returns text that is safe to surface to end users. Unclassified
// causes are hidden because they might embed secrets or internal paths.
func UserMessage(err error) string {
	var e *Error
	if errors.As(err, &e) && e.Message != "" {
		return e.Message
	}
	if err == nil {
		return ""
	}
	return "internal error"
}
