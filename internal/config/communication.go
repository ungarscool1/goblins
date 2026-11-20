package config

// Communication presets. Compact, the default, shows a short summary first and keeps the
// full design one click away; verbose shows every design section expanded.
const (
	VerbosityCompact = "compact"
	VerbosityVerbose = "verbose"
)

// Communication lays out what Goblins writes for people, per project. It never changes what
// is stored, approved or given to agents.
type Communication struct {
	Verbosity string `yaml:"verbosity,omitempty"`
}

func (c Communication) validate(path string, add func(string, ...any)) {
	switch c.Verbosity {
	case "", VerbosityCompact, VerbosityVerbose:
	default:
		add("%s.communication.verbosity: must be compact or verbose", path)
	}
}
