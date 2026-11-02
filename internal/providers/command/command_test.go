package command_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers"
	"github.com/ungarscool1/goblins/internal/providers/command"
	"github.com/ungarscool1/goblins/internal/providers/contract"
	"github.com/ungarscool1/goblins/internal/providers/fake"
)

func TestContract(t *testing.T) {
	p := command.New("v", command.Options{Image: "i", Checks: []command.Check{{Name: "t", Run: "true"}}})
	contract.Validation(t, p, fake.NewProcessRuntime("r", t.TempDir(), ""))
}

func run(t *testing.T, checks ...command.Check) (core.ValidationReport, *fake.ProcessRuntime) {
	t.Helper()
	rt := fake.NewProcessRuntime("r", t.TempDir(), "")
	p := command.New("v", command.Options{Image: "i", Checks: checks})
	rep, err := p.Validate(context.Background(), core.ValidationRequest{WorkloadName: "w", Runtime: rt, Workspace: core.Mount{Volume: "s", Target: "/scratch"}, WorkDir: "/scratch/repo"})
	if err != nil {
		t.Fatal(err)
	}
	return rep, rt
}

func TestPassAndFail(t *testing.T) {
	rep, _ := run(t, command.Check{Name: "a", Run: "echo ok"}, command.Check{Name: "b", Run: "echo boom; exit 3"})
	if rep.Status != core.ValidationFailed || !strings.Contains(rep.Summary, "b") || strings.Contains(rep.Summary, "a,") {
		t.Fatalf("report = %+v", rep)
	}
	if rep.Checks[1].ExitCode != 3 || !strings.Contains(rep.Checks[1].Output, "boom") {
		t.Fatalf("check = %+v", rep.Checks[1])
	}
	rep, _ = run(t, command.Check{Name: "a", Run: "true"})
	if rep.Status != core.ValidationPassed {
		t.Fatalf("report = %+v", rep)
	}
}

func TestChecksHoldNoCredentialsAndNoNetwork(t *testing.T) {
	_, rt := run(t, command.Check{Name: "a", Run: "true"})
	s := rt.Specs[0]
	if len(s.Secrets) != 0 || s.Network != core.NetworkNone {
		t.Fatalf("spec = %+v", s)
	}
}

func TestOutputIsBounded(t *testing.T) {
	rep, _ := run(t, command.Check{Name: "a", Run: `i=0; while [ $i -lt 4000 ]; do echo line-of-output-to-fill-the-buffer; i=$((i+1)); done; exit 1`})
	if n := len(rep.Checks[0].Output); n > 20<<10 {
		t.Fatalf("output = %d bytes", n)
	}
}

func TestFactory(t *testing.T) {
	r := providers.NewRegistry()
	command.Register(r)
	ok := providers.Options{"image": "i", "checks": []any{map[string]any{"name": "n", "run": "r"}}}
	if _, err := providers.As[core.ValidationProvider](r, providers.KindValidation, "command", "v", providers.Deps{}, ok); err != nil {
		t.Fatal(err)
	}
	if _, err := providers.As[core.ValidationProvider](r, providers.KindValidation, "command", "v",
		providers.Deps{}, providers.Options{"image": "i", "manifest_only": true}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []providers.Options{
		{"checks": ok["checks"]},
		{"image": "i"},
		{"image": "i", "checks": []any{map[string]any{"name": "n"}}},
		{"image": "i", "checks": ok["checks"], "network": "host"},
		{"image": "i", "checks": ok["checks"], "timeout": "soon"},
		{"image": "i", "checks": []any{map[string]any{"name": "n", "run": "r", "privileged": true}}},
	} {
		if _, err := providers.As[core.ValidationProvider](r, providers.KindValidation, "command", "v", providers.Deps{}, bad); !core.IsKind(err, core.KindInvalid) {
			t.Fatalf("%v accepted: %v", bad, err)
		}
	}
}

func validate(t *testing.T, o command.Options,
	manifest string) (core.ValidationReport, *fake.ProcessRuntime) {
	t.Helper()
	rt := fake.NewProcessRuntime("r", t.TempDir(), "")
	req := core.ValidationRequest{WorkloadName: "w", Runtime: rt,
		Workspace: core.Mount{Volume: "v", Target: "/w"}, WorkDir: "/w"}
	if manifest != "" {
		req.Manifest = []byte(manifest)
	}
	rep, err := command.New("v", o).Validate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return rep, rt
}

func TestOptionalChecksAreReportedAndDoNotDecide(t *testing.T) {
	rep, _ := run(t, command.Check{Name: "a", Run: "true"},
		command.Check{Name: "extra", Run: "exit 1", Optional: true})
	if rep.Status != core.ValidationPassed ||
		!strings.Contains(rep.Summary, "optional and not passed: extra") {
		t.Fatalf("report = %+v", rep)
	}
	if !rep.Checks[1].Optional || rep.Checks[1].Status != core.ValidationFailed {
		t.Fatalf("check = %+v", rep.Checks[1])
	}
}

func TestEachCheckPinsItsOwnEnvironment(t *testing.T) {
	rep, rt := run(t, command.Check{Name: "a", Run: "true", Stage: "lint", Image: "tool:1.2",
		Network: "egress", Timeout: time.Minute}, command.Check{Name: "b", Run: "true"})
	first, second := rt.Specs[0], rt.Specs[1]
	if first.Image != "tool:1.2" || first.Network != core.NetworkEgress ||
		first.Timeout != time.Minute {
		t.Fatalf("spec = %+v", first)
	}
	if second.Image != "i" || second.Network != core.NetworkNone ||
		second.Timeout != 30*time.Minute {
		t.Fatalf("spec = %+v", second)
	}
	if rep.Checks[0].Image != "tool:1.2" || rep.Checks[0].Stage != "lint" {
		t.Fatalf("check = %+v", rep.Checks[0])
	}
}

func TestMissingKVMStopsARequiredCheckCleanly(t *testing.T) {
	rep, _ := run(t, command.Check{Name: "vms", Run: "true", KVM: true})
	if rep.Status != core.ValidationError || !strings.Contains(rep.Summary, "vms could not start") {
		t.Fatalf("report = %+v", rep)
	}
	rep, _ = run(t, command.Check{Name: "vms", Run: "true", KVM: true, Optional: true},
		command.Check{Name: "a", Run: "true"})
	if rep.Status != core.ValidationPassed || rep.Checks[0].Status != core.ValidationError {
		t.Fatalf("report = %+v", rep)
	}
}

func TestAMockBackendCanRunInsideACheck(t *testing.T) {
	rep, _ := run(t, command.Check{Name: "api", Run: `
(while [ ! -f request ]; do sleep 0.01; done; echo pong > response) &
echo ping > request
while [ ! -f response ]; do sleep 0.01; done
grep -q pong response`})
	if rep.Status != core.ValidationPassed {
		t.Fatalf("report = %+v", rep)
	}
}

func TestManifestReplacesConfiguredChecks(t *testing.T) {
	o := command.Options{Image: "i", Checks: []command.Check{{Name: "configured", Run: "exit 1"}}}
	rep, rt := validate(t, o, "version: 1\nimage: tool:9\nchecks:\n  - {name: m, run: \"true\"}\n")
	if rep.Status != core.ValidationPassed || len(rep.Checks) != 1 || rep.Checks[0].Name != "m" {
		t.Fatalf("report = %+v", rep)
	}
	if rt.Specs[0].Image != "tool:9" {
		t.Fatalf("spec = %+v", rt.Specs[0])
	}
}

func TestManifestCannotAskForMoreThanTheOperatorAllows(t *testing.T) {
	cases := map[string]string{
		"network": "version: 1\nchecks:\n  - {name: m, run: \"true\", network: egress}\n",
		"kvm":     "version: 1\nchecks:\n  - {name: m, run: \"true\", kvm: true}\n",
		"version": "version: 2\nchecks:\n  - {name: m, run: \"true\"}\n",
		"field":   "version: 1\nchecks:\n  - {name: m, run: \"true\", privileged: true}\n",
		"empty":   "version: 1\n",
		"path":    "version: 1\nchecks:\n  - {name: m, run: \"true\", artifacts: [/etc/passwd]}\n",
		"yaml":    "{",
	}
	for name, manifest := range cases {
		t.Run(name, func(t *testing.T) {
			rep, rt := validate(t, command.Options{Image: "i"}, manifest)
			if rep.Status != core.ValidationError || len(rt.Specs) != 0 {
				t.Fatalf("report = %+v, ran %d", rep, len(rt.Specs))
			}
		})
	}
	o := command.Options{Image: "i", ManifestNetwork: true}
	if rep, _ := validate(t, o, cases["network"]); rep.Status != core.ValidationPassed {
		t.Fatalf("report = %+v", rep)
	}
}

func TestNothingToCheckIsNotAPass(t *testing.T) {
	rep, _ := validate(t, command.Options{Image: "i"}, "")
	if rep.Status != core.ValidationError {
		t.Fatalf("report = %+v", rep)
	}
}
