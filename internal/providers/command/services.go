package command

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers"
)

var envName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// testSecrets reads the operator's test secrets: a list of names, each with the environment
// variable holding its value. Values never appear in the configuration.
func testSecrets(deps providers.Deps, opts providers.Options) (map[string]core.Secret, error) {
	raw, _ := opts["test_secrets"].([]any)
	out := map[string]core.Secret{}
	for i, item := range raw {
		m, _ := item.(map[string]any)
		name, _ := m["name"].(string)
		from, _ := m["env"].(string)
		if !envName.MatchString(name) || from == "" {
			return nil, core.Errorf(core.KindInvalid, "command.config",
				"test_secrets[%d] needs a name in capitals and the env variable holding it", i)
		}
		v, ok := "", false
		if deps.Getenv != nil {
			v, ok = deps.Getenv(from)
		}
		if !ok || v == "" {
			return nil, core.Errorf(core.KindInvalid, "command.config",
				"test_secrets[%d]: %s is not set", i, from)
		}
		if len(v) < minSecret {
			// Shorter, it could not be removed from output without mangling everything else.
			return nil, core.Errorf(core.KindInvalid, "command.config",
				"test_secrets[%d]: %s is shorter than %d characters", i, from, minSecret)
		}
		out[name] = core.NewSecret(v)
	}
	return out, nil
}

func (c Check) knownSecrets(known map[string]core.Secret) error {
	for _, s := range c.Secrets {
		if _, ok := known[s]; !ok {
			return fmt.Errorf("check %s: no test secret named %s is configured", c.Name, s)
		}
	}
	return nil
}

func (c Check) validServices() error {
	if len(c.Services) > core.MaxServices {
		return fmt.Errorf("check %s: at most %d services", c.Name, core.MaxServices)
	}
	seen := map[string]bool{}
	for _, s := range c.Services {
		if s.Name == "" || s.Image == "" || seen[s.Name] {
			return fmt.Errorf("check %s: each service needs a unique name and an image", c.Name)
		}
		seen[s.Name] = true
	}
	if len(c.Services) > 0 && c.Network == string(core.NetworkEgress) {
		return fmt.Errorf("check %s: services need a check with no network", c.Name)
	}
	return nil
}

func (c Check) services() []core.Service {
	var out []core.Service
	for _, s := range c.Services {
		out = append(out, core.Service{Name: s.Name, Image: s.Image, Command: s.Command,
			Env: maps.Clone(s.Env)})
	}
	return out
}

func (c Check) testSecrets(all map[string]core.Secret) map[string]core.Secret {
	if len(c.Secrets) == 0 {
		return nil
	}
	out := map[string]core.Secret{}
	for _, name := range c.Secrets {
		out[name] = all[name]
	}
	return out
}

// minSecret is the shortest test secret accepted, so that every one can be scrubbed.
const minSecret = 4

// scrub removes test secret values from a check's output, which goes to the ticket and back
// to the implementer. Longest first, so a value inside another is not left half. Every value
// is at least minSecret long, as configuration refuses shorter ones.
func scrub(output string, secrets map[string]core.Secret) string {
	var values []string
	for _, s := range secrets {
		if v := s.Reveal(); v != "" {
			values = append(values, v)
		}
	}
	slices.SortFunc(values, func(a, b string) int { return len(b) - len(a) })
	for _, v := range values {
		output = strings.ReplaceAll(output, v, "[REDACTED]")
	}
	return output
}
