package knowledge

import (
	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers"
)

// Register adds the knowledge sources to a registry.
func Register(r *providers.Registry) {
	r.Register(providers.KindKnowledge, "files", func(name string, _ providers.Deps,
		opts providers.Options) (any, error) {
		return NewFiles(name, opts.String("path"), opts.Strings("globs"))
	})
	r.Register(providers.KindKnowledge, "outline", func(name string, deps providers.Deps,
		opts providers.Options) (any, error) {
		tok, err := token(deps, opts)
		if err != nil {
			return nil, err
		}
		return NewOutline(name, opts.String("url"), tok, opts.Strings("collections"))
	})
	r.Register(providers.KindKnowledge, "confluence", func(name string, deps providers.Deps,
		opts providers.Options) (any, error) {
		tok, err := token(deps, opts)
		if err != nil {
			return nil, err
		}
		auth := "Bearer " + tok.Reveal()
		if e := opts.String("email"); e != "" {
			auth = basic(e, tok.Reveal())
		}
		return NewConfluence(name, opts.String("url"), auth, opts.Strings("spaces"))
	})
}

func token(deps providers.Deps, opts providers.Options) (core.Secret, error) {
	t, err := opts.Secret(deps, "token")
	if err == nil && t.Empty() {
		err = core.Errorf(core.KindInvalid, "knowledge.config",
			"token_env or token_file is required")
	}
	return t, err
}
