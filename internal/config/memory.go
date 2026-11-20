package config

import (
	"slices"
	"strings"
)

// Memory says what agents are told about a project beyond its ticket: its accepted memories,
// and documents from these knowledge sources. See docs/memory.md.
type Memory struct {
	Enabled bool     `yaml:"enabled,omitempty"`
	Sources []string `yaml:"sources,omitempty"`
	// Required sources stop a run when they cannot be read; others are skipped and noted.
	Required []string `yaml:"required,omitempty"`
	// Budget is how many bytes of a prompt memory may take. 8 KiB by default.
	Budget int `yaml:"budget,omitempty"`
	// Repository lists the project's own documentation, as paths in its repository, read
	// through the SCM at the commit a task works from. It is the source "repo".
	Repository []string `yaml:"repository,omitempty"`
}

func (m Memory) validate(path string, set ProviderSet, add func(string, ...any)) {
	for _, s := range m.Sources {
		if _, ok := set[s]; !ok {
			add("%s.memory.sources: references undefined knowledge source %q", path, s)
		}
	}
	for _, g := range m.Repository {
		if g == "" || strings.HasPrefix(g, "/") || slices.Contains(strings.Split(g, "/"), "..") {
			add("%s.memory.repository: %q must be a path inside the repository", path, g)
		}
	}
	if len(m.Repository) > 0 && slices.Contains(m.Sources, "repo") {
		add("%s.memory.sources: \"repo\" is the repository's own documentation, rename that source",
			path)
	}
	for _, s := range m.Required {
		if s == "repo" && len(m.Repository) > 0 {
			continue
		}
		if !slices.Contains(m.Sources, s) {
			add("%s.memory.required: %q is not one of the sources", path, s)
		}
	}
	if m.Budget < 0 || m.Budget > 64<<10 {
		add("%s.memory.budget: must be between 0 and 65536", path)
	}
	if !m.Enabled && (len(m.Sources) > 0 || len(m.Repository) > 0) {
		add("%s.memory: sources are set but memory is not enabled", path)
	}
}
