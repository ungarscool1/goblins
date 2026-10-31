package providers

import (
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

// Deps are the capabilities handed to adapter factories. They deliberately
// exclude the state store and workflow engine: an adapter can only report facts
// and perform external operations, never move a task between states.
type Deps struct {
	Log          *slog.Logger
	Installation string
	DataDir      string
	// Getenv resolves *_env secret references.
	Getenv func(string) (string, bool)
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Clock returns the configured clock.
func (d Deps) Clock() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
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

// Build instantiates an adapter and checks it against the contract version.
func (r *Registry) Build(kind Kind, typ, name string, deps Deps, opts Options) (core.Provider, error) {
	r.mu.RLock()
	f, ok := r.factories[kind][typ]
	r.mu.RUnlock()
	if !ok {
		return nil, core.Errorf(core.KindInvalid, "providers.build", "unknown %s provider %q", kind, typ)
	}
	v, err := f(name, deps, opts)
	if err != nil {
		return nil, err
	}
	p, ok := v.(core.Provider)
	if !ok {
		return nil, core.Errorf(core.KindInternal, "providers.build", "%s provider %q does not implement core.Provider", kind, typ)
	}
	if got := p.Info().ContractVersion; got != core.ContractVersion {
		return nil, core.Errorf(core.KindInvalid, "providers.build", "%s provider %q targets contract version %d, this build requires %d", kind, typ, got, core.ContractVersion)
	}
	return p, nil
}

// As builds an adapter and asserts its contract type T.
func As[T core.Provider](r *Registry, kind Kind, typ, name string, deps Deps, opts Options) (T, error) {
	var zero T
	p, err := r.Build(kind, typ, name, deps, opts)
	if err != nil {
		return zero, err
	}
	t, ok := p.(T)
	if !ok {
		return zero, core.Errorf(core.KindInternal, "providers.build", "%s provider %q does not implement the %s contract", kind, typ, kind)
	}
	return t, nil
}

// String returns a string option.
func (o Options) String(key string) string {
	s, _ := o[key].(string)
	return s
}

// StringOr returns a string option or a default.
func (o Options) StringOr(key, def string) string {
	if s := o.String(key); s != "" {
		return s
	}
	return def
}

// Int returns an integer option or a default.
func (o Options) Int(key string, def int) int {
	if v, ok := o[key].(int); ok {
		return v
	}
	return def
}

// Bool returns a boolean option or a default.
func (o Options) Bool(key string, def bool) bool {
	if v, ok := o[key].(bool); ok {
		return v
	}
	return def
}

// Strings returns a list-of-strings option.
func (o Options) Strings(key string) []string {
	raw, _ := o[key].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// Secret resolves key_env or key_file. Secrets are never accepted inline.
func (o Options) Secret(deps Deps, key string) (core.Secret, error) {
	if name := o.String(key + "_env"); name != "" {
		if deps.Getenv != nil {
			if v, ok := deps.Getenv(name); ok && v != "" {
				return core.NewSecret(v), nil
			}
		}
		return core.Secret{}, core.Errorf(core.KindInvalid, "providers.secret", "environment variable %s for %s is not set", name, key)
	}
	if path := o.String(key + "_file"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return core.Secret{}, core.Errorf(core.KindInvalid, "providers.secret", "cannot read file for %s", key)
		}
		return core.NewSecret(strings.TrimRight(string(data), "\r\n")), nil
	}
	return core.Secret{}, nil
}
