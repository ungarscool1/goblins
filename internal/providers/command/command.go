// Package command is a ValidationProvider that runs project-defined shell commands in a
// workload with no credentials and, by default, no network.
package command

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers"
	rt "github.com/ungarscool1/goblins/internal/runtime"
)

const (
	typeName  = "command"
	maxOutput = 16 << 10
)

// Check is one named command.
type Check struct {
	Name string
	Run  string
}

// Options configures a provider.
type Options struct {
	Image        string
	Checks       []Check
	Network      core.NetworkPolicy
	Timeout      time.Duration
	WritableRoot bool
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
		raw, _ := opts["checks"].([]any)
		for _, item := range raw {
			m, _ := item.(map[string]any)
			n, _ := m["name"].(string)
			run, _ := m["run"].(string)
			if n == "" || run == "" {
				return nil, core.Errorf(core.KindInvalid, "command.config", "each check needs a name and a run command")
			}
			o.Checks = append(o.Checks, Check{Name: n, Run: run})
		}
		if len(o.Checks) == 0 {
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
// status, which says nothing about the code; one that runs and fails is a failure.
func (p *Provider) Validate(ctx context.Context, req core.ValidationRequest) (core.ValidationReport, error) {
	report := core.ValidationReport{Status: core.ValidationPassed}
	for i, c := range p.opts.Checks {
		spec := core.WorkloadSpec{
			Name: fmt.Sprintf("%s-%d", req.WorkloadName, i), Image: p.opts.Image, Command: []string{"sh", "-c", c.Run},
			WorkDir: req.WorkDir, Mounts: []core.Mount{req.Workspace}, Network: p.opts.Network, Labels: req.Labels, Timeout: p.opts.Timeout,
			Env:      map[string]string{"HOME": "/tmp", "CI": "true"},
			Security: core.SecurityProfile{ReadOnlyRoot: !p.opts.WritableRoot},
		}
		res, err := rt.RunToCompletion(ctx, req.Runtime, spec)
		if err != nil {
			if core.IsKind(err, core.KindCancelled) || core.IsKind(err, core.KindTransient) {
				return report, err
			}
			report.Status = core.ValidationError
			report.Summary = fmt.Sprintf("check %s could not start: %s", c.Name, core.UserMessage(err))
			return report, nil
		}
		output := res.Stdout + res.Stderr
		if len(output) > maxOutput {
			output = "[... truncated ...]\n" + output[len(output)-maxOutput:]
		}
		check := core.ValidationCheck{Name: c.Name, Status: core.ValidationPassed, ExitCode: res.Status.ExitCode, Output: output}
		if res.Status.Phase != core.WorkloadSucceeded {
			check.Status = core.ValidationFailed
			report.Status = core.ValidationFailed
		}
		report.Checks = append(report.Checks, check)
	}
	var failed []string
	for _, c := range report.Checks {
		if c.Status != core.ValidationPassed {
			failed = append(failed, c.Name)
		}
	}
	if len(failed) > 0 {
		report.Summary = "failed: " + strings.Join(failed, ", ")
	} else {
		report.Summary = fmt.Sprintf("%d checks passed", len(report.Checks))
	}
	return report, nil
}
