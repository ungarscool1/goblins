package contract

import (
	"context"
	"slices"
	"testing"

	"github.com/ungarscool1/goblins/internal/core"
)

// EnvironmentHarness supplies an environment provider whose steps all succeed.
type EnvironmentHarness struct {
	Provider core.EnvironmentProvider
	Runtime  core.ExecutionRuntime
	// Workspace holds the commit steps see.
	Workspace core.Mount
	WorkDir   string
}

var phaseKinds = map[core.EnvironmentPhase][]core.OperationKind{
	core.PhaseAcquire:  {core.OperationRead, core.OperationMutate},
	core.PhaseDeploy:   {core.OperationRead, core.OperationMutate},
	core.PhaseVerify:   {core.OperationRead},
	core.PhaseEvidence: {core.OperationRead},
	core.PhaseRelease:  {core.OperationRead, core.OperationMutate, core.OperationDestructive},
}

// EnvironmentProvider runs the environment conformance suite.
func EnvironmentProvider(t *testing.T, build func(t *testing.T) EnvironmentHarness) {
	ctx := context.Background()
	t.Run("info", func(t *testing.T) { checkInfo(t, build(t).Provider, core.ProviderEnvironment) })

	t.Run("steps fit their phase", func(t *testing.T) {
		h := build(t)
		names := map[string]bool{}
		deploys := 0
		for _, s := range h.Provider.Steps() {
			if names[s.Name] || s.Name == "" {
				t.Errorf("step names must be unique: %q", s.Name)
			}
			names[s.Name] = true
			if !slices.Contains(phaseKinds[s.Phase], s.Kind) {
				t.Errorf("step %s: a %s step cannot be %s", s.Name, s.Phase, s.Kind)
			}
			if s.Phase == core.PhaseDeploy {
				deploys++
			}
		}
		if deploys == 0 {
			t.Error("no deploy step")
		}
	})

	t.Run("every step runs and reports", func(t *testing.T) {
		h := build(t)
		env := core.Environment{Provider: h.Provider.Info().Name, Target: "contract",
			Ephemeral: true}
		if ts := h.Provider.Targets(); len(ts) > 0 {
			env = core.Environment{Provider: env.Provider, Target: ts[0]}
		}
		for i, s := range h.Provider.Steps() {
			res, err := h.Provider.RunStep(ctx, core.StepRequest{Env: env, Step: s, Holder: "task",
				SHA: "abc", Runtime: h.Runtime, Workspace: h.Workspace, WorkDir: h.WorkDir,
				WorkloadName: uniqueName(t, "env") + string(rune('a'+i)),
				Labels:       map[string]string{"goblins.test": "contract"}})
			if err != nil || !res.OK {
				t.Fatalf("step %s: %+v, %v", s.Name, res, err)
			}
		}
	})
}
