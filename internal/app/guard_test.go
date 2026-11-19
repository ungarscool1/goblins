package app

import (
	"testing"

	"github.com/ungarscool1/goblins/internal/config"
	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers/claude"
	"github.com/ungarscool1/goblins/internal/providers/fake"
)

func TestStagesCanOnlyNameToolsTheirAgentOffers(t *testing.T) {
	withDocs := claude.New("c", claude.Options{Image: "i",
		MCPServers: map[string]claude.MCPServer{"docs": {Command: "/opt/mcp/docs"}}})
	if err := checkTools(config.Stage{Agent: "c", Tools: []string{"docs"}}, withDocs); err != nil {
		t.Fatal(err)
	}
	if err := checkTools(config.Stage{Agent: "c", Tools: []string{"shell"}}, withDocs); err == nil {
		t.Fatal("a tool the agent does not offer was accepted")
	}
	err := checkTools(config.Stage{Agent: "f", Tools: []string{"docs"}}, fake.NewAgent("f"))
	if !core.IsKind(err, core.KindUnsupported) {
		t.Fatalf("an agent without tools: %v", err)
	}
}
