package providers

import (
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
)

// Deps are the capabilities handed to adapter factories. They deliberately
// exclude the state store and workflow engine: an adapter can only report facts
// and perform external operations, never move a task between states.
type Deps struct {
	Log          *slog.Logger
	Installation string
	DataDir      string
	// Secret resolves a secret reference from the environment or a file.
	Getenv func(string) (string, bool)
}

// Options is the free-form configuration of one adapter instance.
type Options map[string]any

// Factory constructs an adapter instance. The result must implement the core
// contract matching the Kind it was registered for.
type Factory func(name string, deps Deps, opts Options) (any, error)

// Registry maps (kind, type) to adapter factories.
type Registry struct {
	mu        sync.RWMutex
	factories map[Kind]map[string]Factory
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{factories: map[Kind]map[string]Factory{}} }

// Register adds a factory and panics on duplicates, which are programming errors.
func (r *Registry) Register(kind Kind, typ string, f Factory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.factories[kind] == nil {
		r.factories[kind] = map[string]Factory{}
	}
	if _, dup := r.factories[kind][typ]; dup {
		panic(fmt.Sprintf("providers: duplicate registration %s/%s", kind, typ))
	}
	r.factories[kind][typ] = f
}

// Known reports whether a type is registered for kind.
func (r *Registry) Known(kind Kind, typ string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.factories[kind][typ]
	return ok
}

// Types lists registered types for kind in sorted order.
func (r *Registry) Types(kind Kind) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Sorted(maps.Keys(r.factories[kind]))
}

// Build instantiates an adapter.
func (r *Registry) Build(kind Kind, typ, name string, deps Deps, opts Options) (any, error) {
	r.mu.RLock()
	f, ok := r.factories[kind][typ]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown %s provider %q", kind, typ)
	}
	return f(name, deps, opts)
}
