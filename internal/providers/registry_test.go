package providers_test

import (
	"testing"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers"
	"github.com/ungarscool1/goblins/internal/providers/fake"
)

type oldAdapter struct{}

func (oldAdapter) Info() core.ProviderInfo {
	return core.ProviderInfo{Kind: core.ProviderTracker, Type: "old", Name: "o", ContractVersion: core.ContractVersion + 1}
}

func TestRegistry(t *testing.T) {
	r := providers.NewRegistry()
	fake.Register(r, fake.NewWorld())
	if !r.Known(providers.KindTracker, "fake") || r.Known(providers.KindTracker, "smoke") {
		t.Fatal("Known wrong")
	}
	if tr, err := providers.As[core.IssueTracker](r, providers.KindTracker, "fake", "x", providers.Deps{}, nil); err != nil || tr.Info().Name != "x" {
		t.Fatalf("As: %v", err)
	}
	if _, err := providers.As[core.IssueTracker](r, providers.KindSCM, "fake", "x", providers.Deps{}, nil); !core.IsKind(err, core.KindInternal) {
		t.Fatalf("wrong contract accepted: %v", err)
	}
	if _, err := r.Build(providers.KindTracker, "smoke", "x", providers.Deps{}, nil); !core.IsKind(err, core.KindInvalid) {
		t.Fatalf("unknown type: %v", err)
	}
	r.Register(providers.KindTracker, "old", func(string, providers.Deps, providers.Options) (any, error) { return oldAdapter{}, nil })
	if _, err := r.Build(providers.KindTracker, "old", "o", providers.Deps{}, nil); !core.IsKind(err, core.KindInvalid) {
		t.Fatalf("wrong contract version accepted: %v", err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate registration must panic")
		}
	}()
	r.Register(providers.KindTracker, "old", nil)
}

func TestOptionsSecrets(t *testing.T) {
	deps := providers.Deps{Getenv: func(k string) (string, bool) { return "s3cret", k == "MY_VAR" }}
	s, err := providers.Options{"token_env": "MY_VAR"}.Secret(deps, "token")
	if err != nil || s.Reveal() != "s3cret" {
		t.Fatalf("secret = %v", err)
	}
	if _, err := (providers.Options{"token_env": "MISSING"}).Secret(deps, "token"); !core.IsKind(err, core.KindInvalid) {
		t.Fatalf("missing env: %v", err)
	}
	if s, err := (providers.Options{}).Secret(deps, "token"); err != nil || !s.Empty() {
		t.Fatalf("absent secret: %v", err)
	}
}
