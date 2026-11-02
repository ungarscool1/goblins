package command

import (
	"bytes"
	"fmt"
	"path"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/ungarscool1/goblins/internal/core"
)

const manifestVersion = 1

// manifest is a project's checks as kept in its repository, read from the base commit. A pull
// request changing it therefore has no effect on its own checks.
type manifest struct {
	Version int     `yaml:"version"`
	Image   string  `yaml:"image"`
	Checks  []Check `yaml:"checks"`
}

// parseManifest reads a manifest. What a repository may ask for stops at what the operator
// allows: network access and KVM stay their decision.
func parseManifest(raw []byte, o Options) ([]Check, error) {
	var m manifest
	if err := decodeStrict(raw, &m); err != nil {
		return nil, fmt.Errorf("unreadable: %w", err)
	}
	if m.Version != manifestVersion {
		return nil, fmt.Errorf("version must be %d", manifestVersion)
	}
	if len(m.Checks) == 0 {
		return nil, fmt.Errorf("it has no checks")
	}
	for i := range m.Checks {
		c := &m.Checks[i]
		if err := c.valid(); err != nil {
			return nil, err
		}
		if c.Network == string(core.NetworkEgress) && !o.ManifestNetwork {
			return nil, fmt.Errorf("check %s asks for network access, "+
				"which this installation does not allow manifests", c.Name)
		}
		if c.KVM && !o.ManifestKVM {
			return nil, fmt.Errorf("check %s asks for KVM, "+
				"which this installation does not allow manifests", c.Name)
		}
		if c.Image == "" {
			c.Image = m.Image
		}
	}
	return m.Checks, nil
}

func decodeStrict(raw []byte, into any) error {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	return dec.Decode(into)
}

// valid rejects a check that could not mean what it says.
func (c Check) valid() error {
	if c.Name == "" || c.Run == "" {
		return fmt.Errorf("each check needs a name and a run command")
	}
	switch c.Network {
	case "", string(core.NetworkNone), string(core.NetworkEgress):
	default:
		return fmt.Errorf("check %s: unknown network %q", c.Name, c.Network)
	}
	if c.Timeout < 0 {
		return fmt.Errorf("check %s: negative timeout", c.Name)
	}
	if len(c.Artifacts) > maxArtifacts {
		return fmt.Errorf("check %s: at most %d artifacts", c.Name, maxArtifacts)
	}
	for _, a := range c.Artifacts {
		if a == "" || path.IsAbs(a) || strings.HasPrefix(a, "-") || path.Clean(a) != a ||
			a == ".." || strings.HasPrefix(a, "../") {
			return fmt.Errorf("check %s: artifact %q must be a plain path inside the repository",
				c.Name, a)
		}
	}
	return nil
}

// script runs a check and then shows what it left behind, which often says more about a
// failure than its output. The check's own exit code stands.
func (c Check) script() (cmd []string, env map[string]string) {
	if len(c.Artifacts) == 0 {
		return []string{"sh", "-c", c.Run}, nil
	}
	env = map[string]string{"GOBLINS_CHECK": c.Run,
		"GOBLINS_ARTIFACTS": strings.Join(c.Artifacts, "\n")}
	script := fmt.Sprintf(`sh -c "$GOBLINS_CHECK"
rc=$?
printf '%%s\n' "$GOBLINS_ARTIFACTS" | while IFS= read -r f; do
  [ -f "./$f" ] || continue
  printf '\n--- artifact: %%s ---\n' "$f"
  tail -c %d -- "./$f"
done
exit $rc`, maxArtifactBytes)
	return []string{"sh", "-c", script}, env
}

func (c Check) timeout(fallback time.Duration) time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return fallback
}
