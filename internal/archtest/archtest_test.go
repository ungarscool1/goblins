// Package archtest enforces architecture rules that the compiler cannot.
package archtest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/ungarscool1/goblins"

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

// goFiles walks a package tree, skipping nothing: tests in core packages must
// stay provider neutral as well.
func goFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".go") {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// corePackages may not know about any concrete product.
var corePackages = []string{"internal/core", "internal/workflow", "internal/storage", "internal/api", "internal/observability", "internal/scheduler", "internal/pipeline"}

var productName = regexp.MustCompile(`(?i)\b(cloudforge|leocloud|github|gitlab|jira|claude|anthropic|codex|openai|docker|kubernetes|k8s)\b`)

func TestCorePackagesAreProductNeutral(t *testing.T) {
	root := repoRoot(t)
	for _, pkg := range corePackages {
		for _, file := range goFiles(t, filepath.Join(root, pkg)) {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			for i, line := range strings.Split(string(data), "\n") {
				// Import paths name hosting sites, not behavior.
				if m := productName.FindString(line); m != "" && !strings.Contains(line, `"github.com/`) && !strings.Contains(line, module) {
					t.Errorf("%s:%d mentions product %q", file, i+1, m)
				}
			}
		}
	}
}

func imports(t *testing.T, file string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		out = append(out, p)
	}
	return out
}

// forbiddenImports lists dependency rules between packages. Non-test code in a
// key may not import any package prefixed by its values.
var forbiddenImports = map[string][]string{
	// Core depends on nothing else in the module except observability.
	"internal/core": {module + "/internal/providers", module + "/internal/workflow", module + "/internal/storage", module + "/internal/scheduler", module + "/internal/api", module + "/internal/config", module + "/internal/app"},
	// Workflow logic only sees contracts, never concrete adapters.
	"internal/workflow":  {module + "/internal/providers", module + "/internal/storage", module + "/internal/api", module + "/internal/app"},
	"internal/scheduler": {module + "/internal/providers", module + "/internal/storage", module + "/internal/api", module + "/internal/app"},
	"internal/pipeline":  {module + "/internal/providers", module + "/internal/storage", module + "/internal/api", module + "/internal/app"},
	// Adapters cannot reach the state machine or storage, so they cannot drive
	// workflow transitions directly.
	"internal/providers": {module + "/internal/workflow", module + "/internal/storage", module + "/internal/scheduler", module + "/internal/pipeline", module + "/internal/app", module + "/internal/api"},
}

func TestDependencyRules(t *testing.T) {
	root := repoRoot(t)
	for pkg, banned := range forbiddenImports {
		for _, file := range goFiles(t, filepath.Join(root, pkg)) {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			for _, imp := range imports(t, file) {
				for _, b := range banned {
					if imp == b || strings.HasPrefix(imp, b+"/") {
						t.Errorf("%s imports forbidden package %s", file, imp)
					}
				}
			}
		}
	}
}
