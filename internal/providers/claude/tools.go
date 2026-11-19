package claude

import (
	"encoding/json"
	"maps"
	"regexp"
	"slices"

	"go.yaml.in/yaml/v3"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers"
)

// MCPServer is an MCP server the agent image already contains. Goblins never installs one:
// what is offered is what an operator built into the image, at a version they chose.
type MCPServer struct {
	Command string            `yaml:"command" json:"command"`
	Args    []string          `yaml:"args" json:"args,omitempty"`
	Env     map[string]string `yaml:"env" json:"env,omitempty"`
}

var toolName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

func mcpServers(opts providers.Options) (map[string]MCPServer, error) {
	raw, ok := opts["mcp_servers"]
	if !ok {
		return nil, nil
	}
	b, _ := yaml.Marshal(raw)
	var out map[string]MCPServer
	if err := yaml.Unmarshal(b, &out); err != nil {
		return nil, core.Errorf(core.KindInvalid, "claude.config", "unreadable mcp_servers")
	}
	for name, s := range out {
		if !toolName.MatchString(name) || s.Command == "" {
			return nil, core.Errorf(core.KindInvalid, "claude.config",
				"mcp_servers.%s needs a lower case name and a command", name)
		}
	}
	return out, nil
}

// Tools implements core.ToolCatalog.
func (a *Agent) Tools() []string { return slices.Sorted(maps.Keys(a.opts.MCPServers)) }

// withTools gives a run the MCP servers its stage names, and nothing else: the configuration
// holds only those, --strict-mcp-config ignores any other, and the allowed tools list only
// theirs.
func (a *Agent) withTools(cliArgs, names []string) ([]string, error) {
	if len(names) == 0 {
		return cliArgs, nil
	}
	servers := map[string]MCPServer{}
	for _, n := range names {
		s, ok := a.opts.MCPServers[n]
		if !ok {
			return nil, core.Unsupported(a.Info(), core.CapAgentTools)
		}
		servers[n] = s
	}
	cfg, err := json.Marshal(map[string]any{"mcpServers": servers})
	if err != nil {
		return nil, core.Wrap(core.KindInternal, "claude.tools", err)
	}
	out := slices.Clone(cliArgs)
	for i, v := range out {
		if v == "--allowedTools" && i+1 < len(out) {
			for _, n := range names {
				out[i+1] += ",mcp__" + n
			}
		}
	}
	return append(out, "--mcp-config", string(cfg)), nil
}
