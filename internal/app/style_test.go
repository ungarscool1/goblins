package app

import (
	"testing"

	"github.com/ungarscool1/goblins/internal/config"
	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/workflow"
)

func TestDesignStyle(t *testing.T) {
	rich := core.ProviderInfo{Capabilities: []core.Capability{core.CapTrackerCollapsible}}
	plain := core.ProviderInfo{}
	for _, c := range []struct {
		verbosity string
		tracker   core.ProviderInfo
		want      workflow.DesignStyle
	}{
		{"", rich, workflow.DesignCompact},
		{config.VerbosityCompact, rich, workflow.DesignCompact},
		{"", plain, workflow.DesignCompactPlain},
		{config.VerbosityVerbose, rich, workflow.DesignVerbose},
		{config.VerbosityVerbose, plain, workflow.DesignVerbose},
	} {
		got := designStyle(config.Communication{Verbosity: c.verbosity}, c.tracker)
		if got != c.want {
			t.Errorf("%q, %v: got %d, want %d", c.verbosity, c.tracker.Capabilities, got, c.want)
		}
	}
}
