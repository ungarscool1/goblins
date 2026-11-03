package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ungarscool1/goblins/internal/config"
)

func TestConsoleTokensAreReadByReferenceAndMustBeLong(t *testing.T) {
	long := strings.Repeat("k", 40)
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(long+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := func(k string) (string, bool) {
		return map[string]string{"LONG": long, "SHORT": "too-short"}[k], true
	}
	ops, err := consoleOperators([]config.Operator{
		{Name: "a", Role: config.RoleOperator, TokenEnv: "LONG"},
		{Name: "b", Role: config.RoleViewer, TokenFile: file}}, env)
	if err != nil || len(ops) != 2 || !ops[0].CanAct || ops[1].CanAct ||
		ops[1].Token.Reveal() != long {
		t.Fatalf("operators = %+v, %v", ops, err)
	}
	for _, op := range []config.Operator{{Name: "c", TokenEnv: "SHORT"},
		{Name: "d", TokenEnv: "MISSING"}, {Name: "e", TokenFile: file + ".missing"}} {
		if _, err := consoleOperators([]config.Operator{op}, env); err == nil {
			t.Errorf("%+v was accepted", op)
		} else if strings.Contains(err.Error(), long) {
			t.Errorf("the error shows a token: %v", err)
		}
	}
}
