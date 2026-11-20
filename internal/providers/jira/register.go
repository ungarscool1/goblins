package jira

import (
	"path/filepath"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers"
)

// Register adds the Jira adapters to a registry.
func Register(r *providers.Registry) {
	r.Register(providers.KindTracker, typeName, func(name string, deps providers.Deps,
		opts providers.Options) (any, error) {
		c, err := clientFromOptions(deps, opts)
		if err != nil {
			return nil, err
		}
		return NewTracker(name, c, TrackerOptions{Projects: opts.Strings("projects"),
			Write: opts.Bool("write", false), Agents: opts.Strings("agents"), Log: deps.Log})
	})
	r.Register(providers.KindEventSource, typeName, func(name string, deps providers.Deps,
		opts providers.Options) (any, error) {
		c, err := clientFromOptions(deps, opts)
		if err != nil {
			return nil, err
		}
		return NewSource(name, c, opts.Strings("projects"))
	})
}

func clientFromOptions(deps providers.Deps, opts providers.Options) (*Client, error) {
	auth, err := authFromOptions(deps, opts)
	if err != nil {
		return nil, err
	}
	var d Deployment
	switch v := opts.String("deployment"); v {
	case "", "auto":
	case "cloud":
		d = DeploymentCloud
	case "data_center":
		d = DeploymentDataCenter
	default:
		return nil, core.Errorf(core.KindInvalid, "jira.client",
			"deployment must be auto, cloud or data_center, not %q", v)
	}
	return NewClient(ClientOptions{URL: opts.String("url"), Auth: auth, Deployment: d,
		Now: deps.Now})
}

// authFromOptions picks the credentials. Each acts as one person, with that person's access:
// there is no administrator mode.
func authFromOptions(deps providers.Deps, opts providers.Options) (Auth, error) {
	switch mode := opts.String("auth"); mode {
	case "api_token":
		tok, err := required(deps, opts, "token")
		if err != nil {
			return nil, err
		}
		if opts.String("email") == "" {
			return nil, core.Errorf(core.KindInvalid, "jira.auth", "api_token needs email")
		}
		return BasicToken{Email: opts.String("email"), Token: tok}, nil
	case "bearer":
		tok, err := required(deps, opts, "token")
		if err != nil {
			return nil, err
		}
		return Bearer{Token: tok}, nil
	case "oauth":
		secret, err := required(deps, opts, "client_secret")
		if err != nil {
			return nil, err
		}
		file := opts.String("refresh_token_file")
		if opts.String("client_id") == "" || file == "" {
			return nil, core.Errorf(core.KindInvalid, "jira.auth",
				"oauth needs client_id and refresh_token_file")
		}
		if !filepath.IsAbs(file) {
			file = filepath.Join(deps.DataDir, file)
		}
		return &OAuth{TokenURL: opts.String("token_url"), ClientID: opts.String("client_id"),
			ClientSecret: secret, RefreshFile: file, Now: deps.Now}, nil
	default:
		return nil, core.Errorf(core.KindInvalid, "jira.auth",
			"auth must be api_token, bearer or oauth, not %q", mode)
	}
}

func required(deps providers.Deps, opts providers.Options, key string) (core.Secret, error) {
	s, err := opts.Secret(deps, key)
	if err != nil {
		return core.Secret{}, err
	}
	if s.Empty() {
		return core.Secret{}, core.Errorf(core.KindInvalid, "jira.auth",
			"%s_env or %s_file is required",
			key, key)
	}
	return s, nil
}
