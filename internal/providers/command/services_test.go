package command_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers"
	"github.com/ungarscool1/goblins/internal/providers/command"
	"github.com/ungarscool1/goblins/internal/providers/fake"
)

func withServices(t *testing.T, o command.Options) (core.ValidationReport, *fake.ProcessRuntime) {
	t.Helper()
	rt := fake.NewProcessRuntime("r", t.TempDir(), "")
	rt.ExtraCaps = []core.Capability{core.CapRuntimeServices}
	o.Image = "i"
	rep, err := command.New("v", o).Validate(context.Background(), core.ValidationRequest{
		WorkloadName: "w", Runtime: rt, Workspace: core.Mount{Volume: "s", Target: "/scratch"},
		WorkDir: "/scratch/repo"})
	if err != nil {
		t.Fatal(err)
	}
	return rep, rt
}

// A check gets its services and the test secrets it names, and nothing it did not ask for.
func TestChecksGetTheirServicesAndTheirTestSecretsOnly(t *testing.T) {
	secrets := map[string]core.Secret{"STAGING_TOKEN": core.NewSecret("stg-0123456789"),
		"OTHER_TOKEN": core.NewSecret("other-0123456789")}
	rep, rt := withServices(t, command.Options{TestSecrets: secrets, Checks: []command.Check{
		{Name: "integration", Run: `echo "token is $STAGING_TOKEN, other is ${OTHER_TOKEN:-unset}"`,
			Secrets: []string{"STAGING_TOKEN"},
			Services: []command.Service{{Name: "db", Image: "postgres:17",
				Env: map[string]string{"POSTGRES_PASSWORD": "x"}}}},
		{Name: "unit", Run: `echo "${STAGING_TOKEN:-none}"`},
	}})
	if rep.Status != core.ValidationPassed {
		t.Fatalf("report = %+v", rep)
	}
	integration, unit := rt.Specs[0], rt.Specs[1]
	if len(integration.Services) != 1 || integration.Services[0].Name != "db" ||
		integration.TestSecrets["STAGING_TOKEN"].Reveal() != "stg-0123456789" ||
		len(integration.TestSecrets) != 1 || integration.Trust != core.TrustSandbox {
		t.Fatalf("integration spec = %+v", integration)
	}
	if len(unit.TestSecrets) != 0 || len(unit.Services) != 0 {
		t.Fatalf("unit spec = %+v", unit)
	}
	out := rep.Checks[0].Output
	if strings.Contains(out, "stg-0123456789") || !strings.Contains(out, "token is [REDACTED]") ||
		!strings.Contains(out, "other is unset") {
		t.Fatalf("output = %q", out)
	}
}

func TestServicesAndSecretsAreTheOperatorsToGrant(t *testing.T) {
	known := map[string]core.Secret{"STAGING_TOKEN": core.NewSecret("stg-0123456789")}
	for name, c := range map[string]struct {
		manifest string
		o        command.Options
		want     string
	}{
		"unknown secret": {"version: 1\nimage: i\nchecks: [{name: a, run: x, secrets: [NOPE]}]",
			command.Options{TestSecrets: known, ManifestSecrets: true},
			"no test secret named NOPE"},
		"secrets not allowed": {"version: 1\nimage: i\nchecks: [{name: a, run: x, " +
			"secrets: [STAGING_TOKEN]}]",
			command.Options{TestSecrets: known}, "does not allow manifests"},
		"services with network": {"version: 1\nimage: i\nchecks: [{name: a, run: x, " +
			"network: egress, services: [{name: db, image: p}]}]",
			command.Options{ManifestNetwork: true},
			"services need a check with no network"},
	} {
		rt := fake.NewProcessRuntime("r", t.TempDir(), "")
		c.o.Image = "i"
		rep, err := command.New("v", c.o).Validate(context.Background(), core.ValidationRequest{
			WorkloadName: "w", Runtime: rt, Manifest: []byte(c.manifest),
			Workspace: core.Mount{Volume: "s", Target: "/scratch"}, WorkDir: "/scratch/repo"})
		if err != nil || rep.Status != core.ValidationError || !strings.Contains(rep.Summary,
			c.want) {
			t.Errorf("%s: %+v, %v", name, rep, err)
		}
	}
}

func TestTestSecretsComeFromTheEnvironment(t *testing.T) {
	reg := providers.NewRegistry()
	command.Register(reg)
	vals := map[string]string{"STG": "stg-value", "SHORT": "abc"}
	env := func(k string) (string, bool) { v, ok := vals[k]; return v, ok }
	opts := providers.Options{"image": "i", "checks": []any{map[string]any{"name": "a", "run": "x",
		"secrets": []any{"STAGING_TOKEN"}}},
		"test_secrets": []any{map[string]any{"name": "STAGING_TOKEN", "env": "STG"}}}
	if _, err := reg.Build(providers.KindValidation, "command", "v", providers.Deps{Getenv: env},
		opts); err != nil {
		t.Fatal(err)
	}
	opts["test_secrets"] = []any{map[string]any{"name": "STAGING_TOKEN", "env": "MISSING"}}
	if _, err := reg.Build(providers.KindValidation, "command", "v", providers.Deps{Getenv: env},
		opts); err == nil {
		t.Fatal("a missing variable was accepted")
	}
	// Too short to scrub from output without mangling it, so refused.
	opts["test_secrets"] = []any{map[string]any{"name": "STAGING_TOKEN", "env": "SHORT"}}
	if _, err := reg.Build(providers.KindValidation, "command", "v", providers.Deps{Getenv: env},
		opts); err == nil || !strings.Contains(err.Error(), "shorter") {
		t.Fatalf("a short secret: %v", err)
	}
}
