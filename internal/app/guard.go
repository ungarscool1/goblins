package app

import (
	"slices"

	"github.com/ungarscool1/goblins/internal/config"
	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers"
	rt "github.com/ungarscool1/goblins/internal/runtime"
)

// guardedRuntime builds a runtime once, wrapped so that sandboxes can never mount a volume an
// agent keeps its login in, and records the agent's login volumes. Every agent is built before
// anything runs, so the list is complete from the first workload.
func guardedRuntime(b *builder, cfg config.Config, name string,
	agent core.AgentProvider) (core.ExecutionRuntime, error) {
	if h, ok := unwrapAgent(agent).(core.CredentialHolder); ok {
		b.creds.Add(h.CredentialVolumes()...)
	}
	if g, ok := b.guarded[name]; ok {
		return g, nil
	}
	raw, err := build[core.ExecutionRuntime](b, providers.KindRuntime, cfg.Runtimes, name)
	if err != nil {
		return nil, err
	}
	g := rt.Guard(raw, &b.creds)
	b.guarded[name] = g
	return g, nil
}

// checkTools refuses a stage naming a tool its agent does not offer, before anything runs: a
// missing tool is a configuration error, never something quietly done without.
func checkTools(s config.Stage, agent core.AgentProvider) error {
	if len(s.Tools) == 0 {
		return nil
	}
	cat, ok := unwrapAgent(agent).(core.ToolCatalog)
	if !ok || !agent.Info().Has(core.CapAgentTools) {
		return core.Unsupported(agent.Info(), core.CapAgentTools)
	}
	for _, t := range s.Tools {
		if !slices.Contains(cat.Tools(), t) {
			return core.Errorf(core.KindInvalid, "app.tools", "agent %s offers no tool %q",
				s.Agent, t)
		}
	}
	return nil
}
