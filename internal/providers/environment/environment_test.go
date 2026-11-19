package environment_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers"
	"github.com/ungarscool1/goblins/internal/providers/contract"
	"github.com/ungarscool1/goblins/internal/providers/environment"
	"github.com/ungarscool1/goblins/internal/providers/fake"
)

var steps = []environment.Step{
	{Name: "plan", Phase: core.PhaseDeploy, Kind: core.OperationRead,
		Run: "echo plan $GOBLINS_ENV_TARGET"},
	{Name: "apply", Phase: core.PhaseDeploy, Kind: core.OperationMutate, Idempotent: true,
		Run: `echo "token=$DEV_TOKEN"`},
	{Name: "health", Phase: core.PhaseVerify, Kind: core.OperationRead, Run: "true"},
}

func TestContract(t *testing.T) {
	t.Run("command", func(t *testing.T) {
		contract.EnvironmentProvider(t, func(t *testing.T) contract.EnvironmentHarness {
			p, err := environment.New("dev", environment.Options{Image: "i", Steps: steps,
				TestSecrets: map[string]core.Secret{"DEV_TOKEN": core.NewSecret("dev-token-123")}})
			if err != nil {
				t.Fatal(err)
			}
			return contract.EnvironmentHarness{Provider: p,
				Runtime:   fake.NewProcessRuntime("r", t.TempDir(), ""),
				Workspace: core.Mount{Volume: "s", Target: "/scratch"}, WorkDir: "/scratch"}
		})
	})
	// An external staging launcher, mocked: no cluster, no Terraform, the same contract.
	t.Run("fake staging", func(t *testing.T) {
		contract.EnvironmentProvider(t, func(t *testing.T) contract.EnvironmentHarness {
			return contract.EnvironmentHarness{Provider: &fake.Environment{Name: "staging",
				TargetsV: []string{"staging-1"}}}
		})
	})
}

func TestStepsAreSandboxesWithDevCredentialsOnly(t *testing.T) {
	p, _ := environment.New("dev", environment.Options{Image: "i", Steps: steps,
		TestSecrets: map[string]core.Secret{"DEV_TOKEN": core.NewSecret("dev-token-123")}})
	rt := fake.NewProcessRuntime("r", t.TempDir(), "")
	res, err := p.RunStep(context.Background(), core.StepRequest{Step: p.Steps()[1],
		Env: core.Environment{Target: "dev-1"}, Runtime: rt, WorkloadName: "w",
		Workspace: core.Mount{Volume: "s", Target: "/scratch"}, WorkDir: "/scratch"})
	if err != nil || !res.OK || strings.Contains(res.Output, "dev-token-123") {
		t.Fatalf("result = %+v, %v", res, err)
	}
	s := rt.Specs[0]
	if s.Trust != core.TrustSandbox || len(s.Secrets) != 0 || !s.Mounts[0].ReadOnly ||
		s.Env["GOBLINS_ENV_TARGET"] != "dev-1" {
		t.Fatalf("spec = %+v", s)
	}
}

func TestStepsMustFitTheirPhase(t *testing.T) {
	for name, s := range map[string][]environment.Step{
		"verify that mutates": {steps[1], {Name: "v", Phase: core.PhaseVerify,
			Kind: core.OperationMutate, Run: "x"}},
		"destructive deploy": {{Name: "d", Phase: core.PhaseDeploy, Kind: core.OperationDestructive,
			Run: "x"}},
		"destructive reset, not allowed": {steps[1], {Name: "r", Phase: core.PhaseRelease,
			Kind: core.OperationDestructive, Run: "x"}},
		"no deploy": {{Name: "h", Phase: core.PhaseVerify, Kind: core.OperationRead, Run: "x"}},
	} {
		if _, err := environment.New("dev", environment.Options{Image: "i", Steps: s}); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestConfiguration(t *testing.T) {
	reg := providers.NewRegistry()
	environment.Register(reg)
	opts := providers.Options{"image": "i", "targets": []any{"dev-1"}, "steps": []any{
		map[string]any{"name": "apply", "phase": "deploy", "kind": "mutate", "run": "x"}}}
	p, err := reg.Build(providers.KindEnvironment, "command", "dev", providers.Deps{}, opts)
	if err != nil || p.(core.EnvironmentProvider).Targets()[0] != "dev-1" {
		t.Fatalf("%v, %v", p, err)
	}
	opts["steps"] = []any{map[string]any{"name": "apply", "phase": "deploy", "kind": "mutate",
		"run": "x", "approved": true}}
	if _, err := reg.Build(providers.KindEnvironment, "command", "dev", providers.Deps{},
		opts); err == nil {
		t.Fatal("an unknown step field was accepted")
	}
}
