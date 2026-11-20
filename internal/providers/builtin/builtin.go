// Package builtin registers every adapter compiled into this binary. It is the
// only place that knows the full list of concrete providers.
package builtin

import (
	"github.com/ungarscool1/goblins/internal/providers"
	"github.com/ungarscool1/goblins/internal/providers/claude"
	"github.com/ungarscool1/goblins/internal/providers/codex"
	"github.com/ungarscool1/goblins/internal/providers/command"
	"github.com/ungarscool1/goblins/internal/providers/environment"
	"github.com/ungarscool1/goblins/internal/providers/fake"
	gh "github.com/ungarscool1/goblins/internal/providers/github"
	"github.com/ungarscool1/goblins/internal/providers/jira"
	"github.com/ungarscool1/goblins/internal/providers/knowledge"
	"github.com/ungarscool1/goblins/internal/runtime/docker"
	"github.com/ungarscool1/goblins/internal/runtime/kubernetes"
)

// Registry returns a registry with all built-in adapters. The fake adapters act on the
// given world, which is only ever populated in mock mode.
func Registry(world *fake.World) *providers.Registry {
	r := providers.NewRegistry()
	fake.Register(r, world)
	gh.Register(r)
	claude.Register(r)
	codex.Register(r)
	jira.Register(r)
	docker.Register(r)
	kubernetes.Register(r)
	command.Register(r)
	environment.Register(r)
	knowledge.Register(r)
	return r
}
