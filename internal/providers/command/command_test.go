package command_test

import (
	"context"
	"strings"
	"testing"

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
	for _, bad := range []providers.Options{
		{"checks": ok["checks"]},
		{"image": "i"},
		{"image": "i", "checks": []any{map[string]any{"name": "n"}}},
		{"image": "i", "checks": ok["checks"], "network": "host"},
		{"image": "i", "checks": ok["checks"], "timeout": "soon"},
	} {
		if _, err := providers.As[core.ValidationProvider](r, providers.KindValidation, "command", "v", providers.Deps{}, bad); !core.IsKind(err, core.KindInvalid) {
			t.Fatalf("%v accepted: %v", bad, err)
		}
	}
}
