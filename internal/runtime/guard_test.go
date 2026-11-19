package runtime_test

import (
	"context"
	"testing"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers/fake"
	"github.com/ungarscool1/goblins/internal/runtime"
)

// A mount that forgets to say it holds a login is still refused to a sandbox.
func TestGuardKeepsLoginsFromSandboxes(t *testing.T) {
	var creds runtime.CredentialSet
	creds.Add("claude-config")
	inner := fake.NewRuntime("rt")
	g := runtime.Guard(inner, &creds)
	ctx := context.Background()
	sandbox := core.WorkloadSpec{Name: "check", Image: "i", Command: []string{"x"},
		Network: core.NetworkNone, Mounts: []core.Mount{{Volume: "claude-config", Target: "/c"}}}
	if _, err := g.Create(ctx, sandbox); !core.IsKind(err, core.KindForbidden) {
		t.Fatalf("sandbox mounting a login: %v", err)
	}
	if _, err := inner.Status(ctx, "check"); !core.IsKind(err, core.KindNotFound) {
		t.Fatal("the refused workload was created")
	}
	agent := sandbox
	agent.Name, agent.Trust = "agent", core.TrustCredentialed
	if _, err := g.Create(ctx, agent); err != nil {
		t.Fatalf("agent: %v", err)
	}
	if _, ok := any(g).(core.VolumeLister); !ok {
		t.Fatal("the guard hides volume listing")
	}
}
