package config

import "slices"

// Memory says what agents are told about a project beyond its ticket: its accepted memories,
// and documents from these knowledge sources. See docs/memory.md.
type Memory struct {
	Enabled bool     `yaml:"enabled,omitempty"`
	Sources []string `yaml:"sources,omitempty"`
	// Required sources stop a run when they cannot be read; others are skipped and noted.
	Required []string `yaml:"required,omitempty"`
	// Budget is how many bytes of a prompt memory may take. 8 KiB by default.
	Budget int `yaml:"budget,omitempty"`
}

func (m Memory) validate(path string, set ProviderSet, add func(string, ...any)) {
	for _, s := range m.Sources {
		if _, ok := set[s]; !ok {
			add("%s.memory.sources: references undefined knowledge source %q", path, s)
		}
	}
	for _, s := range m.Required {
		if !slices.Contains(m.Sources, s) {
			add("%s.memory.required: %q is not one of the sources", path, s)
		}
	}
	if m.Budget < 0 || m.Budget > 64<<10 {
		add("%s.memory.budget: must be between 0 and 65536", path)
	}
	if !m.Enabled && len(m.Sources) > 0 {
		add("%s.memory: sources are set but memory is not enabled", path)
	}
}
