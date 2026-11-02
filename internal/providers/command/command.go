// Package command is a ValidationProvider that runs project-defined shell commands in a
// workload with no credentials and, by default, no network.
package command

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers"
	rt "github.com/ungarscool1/goblins/internal/runtime"
)

const (
	typeName  = "command"
	maxOutput = 16 << 10

	maxArtifacts     = 5
	maxArtifactBytes = 4 << 10
)

// Check is one named command.
type Check struct {
	Name string `yaml:"name"`
	Run  string `yaml:"run"`
	// Stage says what kind of check this is, such as lint or integration. It is a label.
	Stage string `yaml:"stage"`
	// Image pins the tools of this check, instead of the provider's image.
	Image   string        `yaml:"image"`
	Timeout time.Duration `yaml:"timeout"`
	// Optional checks are reported, but do not stop a change.
	Optional bool `yaml:"optional"`
	// Network is none or egress. Empty means the provider's setting.
	Network string `yaml:"network"`
	// KVM asks for virtualization. A host or runtime without it refuses before anything runs.
	KVM bool `yaml:"kvm"`
	// Artifacts are files the check leaves in the repository, shown after its output.
	Artifacts []string `yaml:"artifacts"`
}

// Options configures a provider.
type Options struct {
	Image        string
	Checks       []Check
	Network      core.NetworkPolicy
	Timeout      time.Duration
	WritableRoot bool
	// ManifestNetwork and ManifestKVM let a repository's manifest ask for these.
	ManifestNetwork bool
	ManifestKVM     bool
}

// Provider implements core.ValidationProvider.
type Provider struct {
	name string
	opts Options
}

// New returns a provider.
func New(name string, o Options) *Provider {
	if o.Network == "" {
		o.Network = core.NetworkNone
	}
	if o.Timeout == 0 {
		o.Timeout = 30 * time.Minute
	}
	return &Provider{name: name, opts: o}
}

// Register adds the provider to a registry.
func Register(r *providers.Registry) {
	r.Register(providers.KindValidation, typeName, func(name string, _ providers.Deps, opts providers.Options) (any, error) {
		o := Options{Image: opts.String("image"), WritableRoot: opts.Bool("writable_root", false)}
		if o.Image == "" {
			return nil, core.Errorf(core.KindInvalid, "command.config", "image is required")
		}
		switch n := opts.StringOr("network", "none"); n {
		case "none":
			o.Network = core.NetworkNone
		case "egress":
			o.Network = core.NetworkEgress
		default:
			return nil, core.Errorf(core.KindInvalid, "command.config", "unknown network %q", n)
		}
		if t := opts.String("timeout"); t != "" {
			d, err := time.ParseDuration(t)
			if err != nil {
				return nil, core.Errorf(core.KindInvalid, "command.config", "invalid timeout %q", t)
			}
			o.Timeout = d
		}
		o.ManifestNetwork = opts.Bool("manifest_network", false)
		o.ManifestKVM = opts.Bool("manifest_kvm", false)
		raw, err := yaml.Marshal(opts["checks"])
		if err != nil {
			return nil, core.Errorf(core.KindInvalid, "command.config", "unreadable checks")
		}
		if err := decodeStrict(raw, &o.Checks); err != nil {
			return nil, core.Errorf(core.KindInvalid, "command.config",
				"unreadable checks: %s", err)
		}
		for _, c := range o.Checks {
			if err := c.valid(); err != nil {
				return nil, core.Errorf(core.KindInvalid, "command.config", "%s", err)
			}
		}
		if len(o.Checks) == 0 && !opts.Bool("manifest_only", false) {
			return nil, core.Errorf(core.KindInvalid, "command.config", "at least one check is required")
		}
		return New(name, o), nil
	})
}

// Info implements core.Provider.
func (p *Provider) Info() core.ProviderInfo {
	return core.ProviderInfo{Kind: core.ProviderValidation, Type: typeName, Name: p.name, ContractVersion: core.ContractVersion}
}

// Validate implements core.ValidationProvider. A check that cannot be started is an error
// status, which says nothing about the code; one that runs and fails is a failure. A manifest
// replaces the configured checks, and one that is wrong says so instead of checking less.
func (p *Provider) Validate(ctx context.Context, req core.ValidationRequest) (core.ValidationReport, error) {
	checks, problem := p.checksFor(req)
	if problem != "" {
		return core.ValidationReport{Status: core.ValidationError, Summary: problem}, nil
	}
	report := core.ValidationReport{Status: core.ValidationPassed}
	for i, c := range checks {
		check, err := p.runCheck(ctx, req, c, i)
		if err != nil {
			if core.IsKind(err, core.KindCancelled) || core.IsKind(err, core.KindTransient) {
				return report, err
			}
			if !c.Optional {
				report.Status = core.ValidationError
				report.Summary = fmt.Sprintf("check %s could not start: %s", c.Name,
					core.UserMessage(err))
				return report, nil
			}
			check.Status, check.Output = core.ValidationError, core.UserMessage(err)
		}
		if check.Status == core.ValidationFailed && !c.Optional {
			report.Status = core.ValidationFailed
		}
		report.Checks = append(report.Checks, check)
	}
	report.Summary = summarize(report.Checks)
	return report, nil
}

// checksFor picks what to run, or says why nothing can be.
func (p *Provider) checksFor(req core.ValidationRequest) ([]Check, string) {
	checks := p.opts.Checks
	if req.Manifest != nil {
		parsed, err := parseManifest(req.Manifest, p.opts)
		if err != nil {
			return nil, "the validation manifest cannot be used: " + err.Error()
		}
		checks = parsed
	}
	if len(checks) == 0 {
		return nil, "no checks are configured, and the base commit has no manifest"
	}
	return checks, ""
}

func (p *Provider) runCheck(ctx context.Context, req core.ValidationRequest, c Check,
	i int) (core.ValidationCheck, error) {
	image := cmp.Or(c.Image, p.opts.Image)
	check := core.ValidationCheck{Name: c.Name, Stage: c.Stage, Image: image, Optional: c.Optional}
	command, script := c.script()
	env := map[string]string{"HOME": "/tmp", "CI": "true"}
	maps.Copy(env, script)
	network := p.opts.Network
	if c.Network != "" {
		network = core.NetworkPolicy(c.Network)
	}
	spec := core.WorkloadSpec{
		Name: fmt.Sprintf("%s-%d", req.WorkloadName, i), Image: image, Command: command,
		WorkDir: req.WorkDir, Mounts: []core.Mount{req.Workspace}, Network: network,
		Labels: req.Labels, Timeout: c.timeout(p.opts.Timeout), Env: env,
		Security: core.SecurityProfile{ReadOnlyRoot: !p.opts.WritableRoot, KVM: c.KVM},
	}
	res, err := rt.RunToCompletion(ctx, req.Runtime, spec)
	if err != nil {
		return check, err
	}
	output := res.Stdout + res.Stderr
	if len(output) > maxOutput {
		output = "[... truncated ...]\n" + output[len(output)-maxOutput:]
	}
	check.Status, check.ExitCode, check.Output = core.ValidationPassed, res.Status.ExitCode, output
	if res.Status.Phase == core.WorkloadTimedOut {
		check.Output = fmt.Sprintf("the check exceeded its time limit of %s\n%s",
			c.timeout(p.opts.Timeout), output)
	}
	if res.Status.Phase != core.WorkloadSucceeded {
		check.Status = core.ValidationFailed
	}
	return check, nil
}

func summarize(checks []core.ValidationCheck) string {
	var failed, optional []string
	for _, c := range checks {
		switch {
		case c.Status == core.ValidationPassed:
		case c.Optional:
			optional = append(optional, c.Name)
		default:
			failed = append(failed, c.Name)
		}
	}
	summary := fmt.Sprintf("%d checks passed", len(checks)-len(failed)-len(optional))
	if len(failed) > 0 {
		summary = "failed: " + strings.Join(failed, ", ")
	}
	if len(optional) > 0 {
		summary += "; optional and not passed: " + strings.Join(optional, ", ")
	}
	return summary
}
