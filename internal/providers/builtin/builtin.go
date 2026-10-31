// Package builtin registers every adapter compiled into this binary. It is the
// only place that knows the full list of concrete providers.
package builtin

import (
	"github.com/ungarscool1/goblins/internal/providers"
	"github.com/ungarscool1/goblins/internal/providers/fake"
	gh "github.com/ungarscool1/goblins/internal/providers/github"
)

// Registry returns a registry with all built-in adapters.
func Registry() *providers.Registry {
	r := providers.NewRegistry()
	fake.Register(r, fake.NewWorld())
	gh.Register(r)
	return r
}
