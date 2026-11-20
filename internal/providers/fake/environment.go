package fake

import (
	"context"
	"slices"
	"sync"

	"github.com/ungarscool1/goblins/internal/core"
)

// Environment is a scripted development environment, standing in for an external staging
// launcher. Fail decides which steps fail; Calls records what ran, on which target.
type Environment struct {
	Name     string
	TargetsV []string
	StepsV   []core.EnvironmentStep
	Fail     func(step, target string) bool
	// Err, when set, is returned by the step it names, as an interruption would be.
	Err map[string]error
	// OnStep, when set, sees each step's request before it runs.
	OnStep func(core.StepRequest)

	mu    sync.Mutex
	Calls []string
}

// DefaultEnvironmentSteps are a provision, a deploy, a check, diagnostics and a reset.
var DefaultEnvironmentSteps = []core.EnvironmentStep{
	{Name: "provision", Phase: core.PhaseAcquire, Kind: core.OperationMutate, Idempotent: true},
	{Name: "deploy", Phase: core.PhaseDeploy, Kind: core.OperationMutate},
	{Name: "smoke", Phase: core.PhaseVerify, Kind: core.OperationRead},
	{Name: "logs", Phase: core.PhaseEvidence, Kind: core.OperationRead},
	{Name: "reset", Phase: core.PhaseRelease, Kind: core.OperationDestructive, Idempotent: true},
}

// Info implements core.Provider.
func (e *Environment) Info() core.ProviderInfo {
	return info(core.ProviderEnvironment, e.Name, nil)
}

// Targets implements core.EnvironmentProvider.
func (e *Environment) Targets() []string { return slices.Clone(e.TargetsV) }

// Steps implements core.EnvironmentProvider.
func (e *Environment) Steps() []core.EnvironmentStep {
	if e.StepsV == nil {
		return slices.Clone(DefaultEnvironmentSteps)
	}
	return slices.Clone(e.StepsV)
}

// RunStep implements core.EnvironmentProvider.
func (e *Environment) RunStep(_ context.Context, req core.StepRequest) (core.StepResult, error) {
	if e.OnStep != nil {
		e.OnStep(req)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Calls = append(e.Calls, req.Step.Name+"@"+req.Env.Target)
	if err := e.Err[req.Step.Name]; err != nil {
		return core.StepResult{}, err
	}
	if e.Fail != nil && e.Fail(req.Step.Name, req.Env.Target) {
		return core.StepResult{ExitCode: 1, Output: req.Step.Name + " failed"}, nil
	}
	return core.StepResult{OK: true, Output: req.Step.Name + " ok"}, nil
}

// Ran returns what ran so far.
func (e *Environment) Ran() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.Calls)
}
