// Package providers contains the provider registry and adapters. Adapters
// translate external systems into core contracts; they never decide workflow
// transitions.
package providers

import "github.com/ungarscool1/goblins/internal/core"

// Kind identifies a family of interchangeable adapters.
type Kind = core.ProviderKind

const (
	KindTracker      = core.ProviderTracker
	KindSCM          = core.ProviderSCM
	KindEventSource  = core.ProviderEventSource
	KindAgent        = core.ProviderAgent
	KindRuntime      = core.ProviderRuntime
	KindReview       = core.ProviderReview
	KindValidation   = core.ProviderValidation
	KindNotification = core.ProviderNotification
)
