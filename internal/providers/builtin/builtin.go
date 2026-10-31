// Package builtin registers every adapter compiled into this binary. It is the
// only place that knows the full list of concrete providers.
package builtin

import (
	"github.com/ungarscool1/goblins/internal/providers"
	"github.com/ungarscool1/goblins/internal/providers/claude"
	"github.com/ungarscool1/goblins/internal/providers/command"
	"github.com/ungarscool1/goblins/internal/providers/fake"
	gh "github.com/ungarscool1/goblins/internal/providers/github"
	"github.com/ungarscool1/goblins/internal/runtime/docker"
)

// Registry returns a registry with all built-in adapters. The fake adapters act on the
// given world, which is only ever populated in mock mode.
func Registry(world *fake.World) *providers.Registry {
	r := providers.NewRegistry()
	fake.Register(r, world)
	gh.Register(r)
	claude.Register(r)
	docker.Register(r)
	command.Register(r)
	return r
}
