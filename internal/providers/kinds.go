// Package providers contains the provider registry and adapters. Adapters
// translate external systems into core contracts; they never decide workflow
// transitions.
package providers

// Kind identifies a family of interchangeable adapters.
type Kind string

const (
	KindTracker      Kind = "tracker"
	KindSCM          Kind = "scm"
	KindEventSource  Kind = "event_source"
	KindAgent        Kind = "agent"
	KindRuntime      Kind = "runtime"
	KindReview       Kind = "review"
	KindValidation   Kind = "validation"
	KindNotification Kind = "notification"
)
