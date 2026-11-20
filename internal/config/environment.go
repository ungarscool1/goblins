package config

import "time"

// Environment says where a project's commits are deployed and verified after their checks
// pass. See docs/environments.md.
type Environment struct {
	Name string `yaml:"name,omitempty"`
	// Lease is how long a task keeps a shared target between uses. Two hours by default.
	Lease time.Duration `yaml:"lease,omitempty"`
}

func (e Environment) validate(path string, p Project, set ProviderSet, add func(string, ...any)) {
	if e.Name == "" {
		return
	}
	if _, ok := set[e.Name]; !ok {
		add("%s.environment.name: references undefined instance %q", path, e.Name)
	}
	if p.Validation == "" {
		add("%s.environment: needs validation, as only a commit that passed its checks is deployed",
			path)
	}
	if e.Lease < 0 {
		add("%s.environment.lease: cannot be negative", path)
	}
}
