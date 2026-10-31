package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommands(t *testing.T) {
	t.Setenv("GOBLINS_CONFIG", "")
	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"config", "check", "--mock"}, &out, &errb); err != nil {
		t.Fatalf("mock config invalid: %v", err)
	}
	out.Reset()
	if err := run(context.Background(), []string{"config", "render", "--mock"}, &out, &errb); err != nil || !strings.Contains(out.String(), "name: mock") {
		t.Fatalf("render: %v %s", err, out.String())
	}
	if err := run(context.Background(), []string{"version"}, &out, &errb); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"nope"}, &out, &errb); err == nil {
		t.Fatal("unknown command accepted")
	}
	if err := run(context.Background(), []string{"serve"}, &out, &errb); err == nil {
		t.Fatal("serve without configuration accepted")
	}
}

func TestConfigCheckRejectsUnknownProvider(t *testing.T) {
	t.Setenv("GOBLINS_CONFIG", "")
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte("version: 1\ntrackers: {t: {type: smoke-signals}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"config", "check", "--config", path}, &out, &errb)
	if err == nil || !strings.Contains(err.Error(), "smoke-signals") {
		t.Fatalf("err = %v", err)
	}
}

func TestShippedExampleConfigsAreValid(t *testing.T) {
	t.Setenv("GOBLINS_CONFIG", "")
	for _, path := range []string{"../../configs/example.yaml", "../../deploy/compose/goblins.example.yaml"} {
		var out, errb bytes.Buffer
		if err := run(context.Background(), []string{"config", "check", "--config", path}, &out, &errb); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
}
