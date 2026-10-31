// Package builtin registers every adapter compiled into this binary. It is the
// only place that knows the full list of concrete providers.
package builtin

import "github.com/ungarscool1/goblins/internal/providers"

// Registry returns a registry with all built-in adapters.
func Registry() *providers.Registry {
	r := providers.NewRegistry()
	stub := func(string, providers.Deps, providers.Options) (any, error) { return struct{}{}, nil }
	for _, kind := range []providers.Kind{
		providers.KindTracker, providers.KindSCM, providers.KindEventSource, providers.KindAgent,
		providers.KindRuntime, providers.KindReview, providers.KindValidation, providers.KindNotification,
	} {
		r.Register(kind, "fake", stub)
	}
	return r
}
