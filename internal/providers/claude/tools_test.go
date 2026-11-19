package claude

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers"
)

func TestStagesGetOnlyTheToolsTheyName(t *testing.T) {
	a := New("c", Options{Image: "i", MCPServers: map[string]MCPServer{
		"docs":  {Command: "/opt/mcp/docs", Args: []string{"--read-only"}},
		"shell": {Command: "/opt/mcp/shell"}}})
	if !a.Info().Has(core.CapAgentTools) || !slices.Equal(a.Tools(), []string{"docs", "shell"}) {
		t.Fatalf("catalog = %v", a.Tools())
	}
	base := args(core.AgentRequest{Role: core.RolePlanner}, true)
	none, err := a.withTools(base, nil)
	if err != nil || slices.Contains(none, "--mcp-config") {
		t.Fatalf("a stage without tools got some: %v", none)
	}
	got, err := a.withTools(base, []string{"docs"})
	if err != nil {
		t.Fatal(err)
	}
	i := slices.Index(got, "--mcp-config")
	var cfg struct {
		Servers map[string]MCPServer `json:"mcpServers"`
	}
	if i < 0 || json.Unmarshal([]byte(got[i+1]), &cfg) != nil || len(cfg.Servers) != 1 ||
		cfg.Servers["docs"].Command != "/opt/mcp/docs" {
		t.Fatalf("args = %v", got)
	}
	allowed := got[slices.Index(got, "--allowedTools")+1]
	if !strings.HasSuffix(allowed, ",mcp__docs") || strings.Contains(allowed, "shell") ||
		!slices.Contains(got, "--strict-mcp-config") {
		t.Fatalf("allowed = %s", allowed)
	}
	if _, err := a.withTools(base, []string{"evil"}); !core.IsKind(err, core.KindUnsupported) {
		t.Fatalf("an unknown tool: %v", err)
	}
}

func TestMCPConfiguration(t *testing.T) {
	if _, err := mcpServers(providers.Options{"mcp_servers": map[string]any{
		"Bad Name": map[string]any{"command": "x"}}}); err == nil {
		t.Fatal("a bad name was accepted")
	}
	got, err := mcpServers(providers.Options{"mcp_servers": map[string]any{
		"docs": map[string]any{"command": "/opt/mcp/docs", "args": []any{"-v"}}}})
	if err != nil || got["docs"].Args[0] != "-v" {
		t.Fatalf("%v, %v", got, err)
	}
	if New("c", Options{Image: "i"}).Info().Has(core.CapAgentTools) {
		t.Fatal("tools offered without any configured")
	}
}
