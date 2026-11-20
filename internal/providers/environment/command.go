// Package environment holds development environment providers. The command provider runs an
// operator's own steps, such as terraform plan, helm upgrade or a staging launcher, as
// unprivileged workloads holding only dev credentials, so nothing about any infrastructure is
// built into Goblins.
package environment

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers"
	"github.com/ungarscool1/goblins/internal/providers/command"
	rt "github.com/ungarscool1/goblins/internal/runtime"
)

const (
	typeName  = "command"
	maxOutput = 16 << 10
)

// Step is one operator-defined operation.
type Step struct {
	Name       string                `yaml:"name"`
	Phase      core.EnvironmentPhase `yaml:"phase"`
	Kind       core.OperationKind    `yaml:"kind"`
	Idempotent bool                  `yaml:"idempotent"`
	Run        string                `yaml:"run"`
	Timeout    time.Duration         `yaml:"timeout"`
}

// Options configures the provider.
type Options struct {
	Image   string
	Network core.NetworkPolicy
	Timeout time.Duration
	Targets []string
	Steps   []Step
	// TestSecrets are the environment's credentials. They must be scoped to development
	// targets only: the code being deployed can read them.
	TestSecrets map[string]core.Secret
	// AllowDestructive permits destructive release steps, such as a reset.
	AllowDestructive bool
}

// Provider implements core.EnvironmentProvider.
type Provider struct {
	name string
	o    Options
}

// New checks the steps and returns a provider.
func New(name string, o Options) (*Provider, error) {
	if o.Image == "" {
		return nil, core.Errorf(core.KindInvalid, "environment.config", "image is required")
	}
	if o.Network == "" {
		o.Network = core.NetworkEgress
	}
	if o.Timeout == 0 {
		o.Timeout = 30 * time.Minute
	}
	if err := checkSteps(o); err != nil {
		return nil, core.Errorf(core.KindInvalid, "environment.config", "%s", err)
	}
	return &Provider{name: name, o: o}, nil
}

var phaseKinds = map[core.EnvironmentPhase][]core.OperationKind{
	core.PhaseAcquire:  {core.OperationRead, core.OperationMutate},
	core.PhaseDeploy:   {core.OperationRead, core.OperationMutate},
	core.PhaseVerify:   {core.OperationRead},
	core.PhaseEvidence: {core.OperationRead},
	core.PhaseRelease:  {core.OperationRead, core.OperationMutate, core.OperationDestructive},
}

// checkSteps refuses steps whose kind does not fit their phase: a check that changes what it
// checks, or a destructive step anywhere but in release, and only when the operator allows it.
func checkSteps(o Options) error {
	seen := map[string]bool{}
	deploys := 0
	for _, s := range o.Steps {
		kinds, ok := phaseKinds[s.Phase]
		switch {
		case s.Name == "" || s.Run == "" || seen[s.Name]:
			return fmt.Errorf("each step needs a unique name and a run command")
		case !ok:
			return fmt.Errorf("step %s: unknown phase %q", s.Name, s.Phase)
		case !slices.Contains(kinds, s.Kind):
			return fmt.Errorf("step %s: a %s step cannot be %q", s.Name, s.Phase, s.Kind)
		case s.Kind == core.OperationDestructive && !o.AllowDestructive:
			return fmt.Errorf("step %s is destructive, which needs allow_destructive", s.Name)
		}
		seen[s.Name] = true
		if s.Phase == core.PhaseDeploy {
			deploys++
		}
	}
	if deploys == 0 {
		return fmt.Errorf("at least one deploy step is required")
	}
	return nil
}

// Register adds the provider to a registry.
func Register(r *providers.Registry) {
	r.Register(providers.KindEnvironment, typeName, func(name string, deps providers.Deps,
		opts providers.Options) (any, error) {
		o := Options{Image: opts.String("image"), Targets: opts.Strings("targets"),
			AllowDestructive: opts.Bool("allow_destructive", false)}
		switch n := opts.StringOr("network", "egress"); n {
		case "none", "egress":
			o.Network = core.NetworkPolicy(n)
		default:
			return nil, core.Errorf(core.KindInvalid, "environment.config", "unknown network %q", n)
		}
		if t := opts.String("timeout"); t != "" {
			d, err := time.ParseDuration(t)
			if err != nil {
				return nil, core.Errorf(core.KindInvalid, "environment.config", "invalid timeout")
			}
			o.Timeout = d
		}
		raw, _ := yaml.Marshal(opts["steps"])
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&o.Steps); err != nil {
			return nil, core.Errorf(core.KindInvalid, "environment.config", "unreadable steps: %s",
				err)
		}
		secrets, err := command.ParseTestSecrets(deps, opts)
		if err != nil {
			return nil, err
		}
		o.TestSecrets = secrets
		return New(name, o)
	})
}

// Info implements core.Provider.
func (p *Provider) Info() core.ProviderInfo {
	return core.ProviderInfo{Kind: core.ProviderEnvironment, Type: typeName, Name: p.name,
		ContractVersion: core.ContractVersion}
}

// Targets implements core.EnvironmentProvider.
func (p *Provider) Targets() []string { return slices.Clone(p.o.Targets) }

// Steps implements core.EnvironmentProvider.
func (p *Provider) Steps() []core.EnvironmentStep {
	out := make([]core.EnvironmentStep, 0, len(p.o.Steps))
	for _, s := range p.o.Steps {
		out = append(out, core.EnvironmentStep{Name: s.Name, Phase: s.Phase, Kind: s.Kind,
			Idempotent: s.Idempotent})
	}
	return out
}

// RunStep implements core.EnvironmentProvider. The step sees the tested commit read-only, the
// environment's credentials and what it needs to know, and nothing else of Goblins.
func (p *Provider) RunStep(ctx context.Context, req core.StepRequest) (core.StepResult, error) {
	i := slices.IndexFunc(p.o.Steps, func(s Step) bool { return s.Name == req.Step.Name })
	if i < 0 {
		return core.StepResult{}, core.Errorf(core.KindInvalid, "environment.run",
			"no step named %s", req.Step.Name)
	}
	s := p.o.Steps[i]
	timeout := p.o.Timeout
	if s.Timeout > 0 {
		timeout = s.Timeout
	}
	ws := req.Workspace
	ws.ReadOnly = true
	spec := core.WorkloadSpec{Name: req.WorkloadName, Image: p.o.Image,
		Command: []string{"sh", "-c", s.Run}, WorkDir: req.WorkDir, Mounts: []core.Mount{ws},
		Network: p.o.Network, Labels: req.Labels, Timeout: timeout, TestSecrets: p.o.TestSecrets,
		Security: core.SecurityProfile{ReadOnlyRoot: true},
		Env: map[string]string{"HOME": "/tmp", "GOBLINS_ENV": p.name,
			"GOBLINS_ENV_TARGET":    req.Env.Target,
			"GOBLINS_ENV_EPHEMERAL": fmt.Sprint(req.Env.Ephemeral),
			"GOBLINS_STEP":          s.Name, "GOBLINS_SHA": req.SHA, "GOBLINS_TASK": req.Holder}}
	res, err := rt.RunToCompletion(ctx, req.Runtime, spec)
	if err != nil {
		return core.StepResult{}, err
	}
	out := command.Scrub(res.Output(), p.o.TestSecrets)
	if len(out) > maxOutput {
		out = "[... truncated ...]\n" + out[len(out)-maxOutput:]
	}
	return core.StepResult{OK: res.Status.Phase == core.WorkloadSucceeded,
		ExitCode: res.Status.ExitCode, TimedOut: res.Status.Phase == core.WorkloadTimedOut,
		Output: out}, nil
}
